package integration

import (
	"bufio"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"path/filepath"
	"regexp"
	"strconv"
	"text/template"
	"time"

	"os"
	"os/exec"

	"github.com/apprenda/kismatic-platform/pkg/tls"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/cloudflare/cfssl/csr"
	. "github.com/onsi/ginkgo"

	"github.com/jmcvetta/guid"
	homedir "github.com/mitchellh/go-homedir"
)

var guidMaker = guid.SimpleGenerator()

func leaveIt() bool {
	return os.Getenv("LEAVE_ARTIFACTS") != ""
}
func bailBeforeAnsible() bool {
	return os.Getenv("BAIL_BEFORE_ANSIBLE") != ""
}

type NodeCount struct {
	Etcd   uint16
	Master uint16
	Worker uint16
}

func (nc NodeCount) Total() uint16 {
	return nc.Etcd + nc.Master + nc.Worker
}

func GetSSHKeyFile() (string, error) {
	dir, err := homedir.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, ".ssh", "kismatic-integration-testing.pem"), nil
}

func InstallKismaticMini(awsos AWSOSDetails) PlanAWS {
	By("Building a template")
	template, err := template.New("planAWSOverlay").Parse(planAWSOverlay)
	FailIfError(err, "Couldn't parse template")

	By("Making infrastructure")
	etcdNode, etcErr := MakeWorkerNode(awsos.AWSAMI)
	FailIfError(etcErr, "Error making etcd node")

	defer TerminateInstances(etcdNode.Instanceid)

	sshKey, err := GetSSHKeyFile()
	FailIfError(err, "Error getting SSH Key file")

	descErr := WaitForInstanceToStart(awsos.AWSUser, sshKey, &etcdNode)
	masterNode := etcdNode
	workerNode := etcdNode
	FailIfError(descErr, "Error waiting for nodes")
	log.Printf("Created etcd nodes: %v (%v), master nodes %v (%v), workerNodes %v (%v)",
		etcdNode.Instanceid, etcdNode.Publicip,
		masterNode.Instanceid, masterNode.Publicip,
		workerNode.Instanceid, workerNode.Publicip)

	By("Building a plan to set up an overlay network cluster on this hardware")
	nodes := PlanAWS{
		Etcd:                     []AWSNodeDeets{etcdNode},
		Master:                   []AWSNodeDeets{masterNode},
		Worker:                   []AWSNodeDeets{workerNode},
		MasterNodeFQDN:           masterNode.Hostname,
		MasterNodeShortName:      masterNode.Hostname,
		SSHKeyFile:               sshKey,
		SSHUser:                  awsos.AWSUser,
		AllowPackageInstallation: true,
	}

	f, fileErr := os.Create("kismatic-testing.yaml")
	FailIfError(fileErr, "Error waiting for nodes")
	defer f.Close()
	w := bufio.NewWriter(f)
	execErr := template.Execute(w, &nodes)
	FailIfError(execErr, "Error filling in plan template")
	w.Flush()

	By("Validing our plan")
	ver := exec.Command("./kismatic", "install", "validate", "-f", f.Name())
	verbytes, verErr := ver.CombinedOutput()
	verText := string(verbytes)

	FailIfError(verErr, "Error validating plan", verText)

	if bailBeforeAnsible() == true {
		return nodes
	}

	By("Punch it Chewie!")
	app := exec.Command("./kismatic", "install", "apply", "-f", f.Name(), "--verbose")
	app.Stdout = os.Stdout
	app.Stderr = os.Stderr
	appErr := app.Run()

	FailIfError(appErr, "Error applying plan")
	return nodes
}

func InstallKismatic(awsos AWSOSDetails) PlanAWS {
	return InstallBigKismatic(NodeCount{Etcd: 1, Master: 1, Worker: 1}, awsos, false, false, false)
}

func InstallKismaticWithDeps(awsos AWSOSDetails) PlanAWS {
	return InstallBigKismatic(NodeCount{Etcd: 1, Master: 1, Worker: 1}, awsos, true, false, false)
}

func InstallKismaticWithAutoConfiguredDocker(awsos AWSOSDetails) PlanAWS {
	return InstallBigKismatic(NodeCount{Etcd: 1, Master: 1, Worker: 1}, awsos, false, true, false)
}

func InstallKismaticWithCustomDocker(awsos AWSOSDetails) PlanAWS {
	return InstallBigKismatic(NodeCount{Etcd: 1, Master: 1, Worker: 1}, awsos, false, false, true)
}

func InstallBigKismatic(count NodeCount, awsos AWSOSDetails, installDeps, autoConfigureDockerRegistry, setupCustomDocker bool) PlanAWS {
	By("Building a template")
	template, err := template.New("planAWSOverlay").Parse(planAWSOverlay)
	FailIfError(err, "Couldn't parse template")
	if count.Etcd < 1 || count.Master < 1 || count.Worker < 1 {
		Fail("Must have at least 1 of every node type")
	}

	By("Making infrastructure")

	allInstanceIDs := make([]string, count.Total())
	etcdNodes := make([]AWSNodeDeets, count.Etcd)
	masterNodes := make([]AWSNodeDeets, count.Master)
	workerNodes := make([]AWSNodeDeets, count.Worker)

	for i := uint16(0); i < count.Etcd; i++ {
		var etcdErr error
		etcdNodes[i], etcdErr = MakeETCDNode(awsos.AWSAMI)
		FailIfError(etcdErr, "Error making etcd node")
		allInstanceIDs[i] = etcdNodes[i].Instanceid
	}

	for i := uint16(0); i < count.Master; i++ {
		var masterErr error
		masterNodes[i], masterErr = MakeMasterNode(awsos.AWSAMI)
		FailIfError(masterErr, "Error making master node")
		allInstanceIDs[i+count.Etcd] = masterNodes[i].Instanceid
	}

	for i := uint16(0); i < count.Worker; i++ {
		var workerErr error
		workerNodes[i], workerErr = MakeWorkerNode(awsos.AWSAMI)
		FailIfError(workerErr, "Error making worker node")
		allInstanceIDs[i+count.Etcd+count.Master] = workerNodes[i].Instanceid
	}

	defer TerminateInstances(allInstanceIDs...)
	sshKey, err := GetSSHKeyFile()
	FailIfError(err, "Error getting SSH Key file")
	nodes := PlanAWS{
		AllowPackageInstallation: true,
		Etcd:                etcdNodes,
		Master:              masterNodes,
		Worker:              workerNodes,
		MasterNodeFQDN:      masterNodes[0].Hostname,
		MasterNodeShortName: masterNodes[0].Hostname,
		SSHKeyFile:          sshKey,
		SSHUser:             awsos.AWSUser,
		AutoConfiguredDockerRegistry: autoConfigureDockerRegistry,
	}
	descErr := WaitForAllInstancesToStart(&nodes)
	FailIfError(descErr, "Error waiting for nodes")
	log.Printf("%v", nodes.Etcd[0].Publicip)
	PrintNodes(&nodes)

	if setupCustomDocker {
		dockerNode := nodes.Etcd[0]
		nodes.DockerRegistryIP, nodes.DockerRegistryPort, nodes.DockerRegistryCAPath = deployDockerRegistry(dockerNode)
	}

	By("Building a plan to set up an overlay network cluster on this hardware")
	var hdErr error
	nodes.HomeDirectory, hdErr = homedir.Dir()
	FailIfError(hdErr, "Error getting home directory")

	f, fileErr := os.Create("kismatic-testing.yaml")
	FailIfError(fileErr, "Error waiting for nodes")
	defer f.Close()
	w := bufio.NewWriter(f)
	execErr := template.Execute(w, &nodes)
	FailIfError(execErr, "Error filling in plan template")
	w.Flush()

	if installDeps {
		By("Installing some RPMs")
		InstallRPMs(nodes, awsos)
	}

	By("Validing our plan")
	ver := exec.Command("./kismatic", "install", "validate", "-f", f.Name())
	verbytes, verErr := ver.CombinedOutput()
	verText := string(verbytes)

	FailIfError(verErr, "Error validating plan", verText)

	if bailBeforeAnsible() == true {
		return nodes
	}

	By("Punch it Chewie!")
	app := exec.Command("./kismatic", "install", "apply", "-f", f.Name(), "--verbose")
	app.Stdout = os.Stdout
	app.Stderr = os.Stderr
	appErr := app.Run()

	FailIfError(appErr, "Error applying plan")

	return nodes
}

func InstallRPMs(nodes PlanAWS, awsos AWSOSDetails) {
	// time.Sleep(90 * time.Second)

	log.Printf("Prepping repos:")
	RunViaSSH(awsos.CommandsToPrepRepo, awsos.AWSUser,
		append(append(nodes.Etcd, nodes.Master...), nodes.Worker...),
		5*time.Minute)

	log.Printf("Installing Etcd:")
	RunViaSSH(awsos.CommandsToInstallEtcd, awsos.AWSUser,
		nodes.Etcd, 5*time.Minute)

	log.Printf("Installing Docker:")
	RunViaSSH(awsos.CommandsToInstallDocker, awsos.AWSUser,
		append(nodes.Master, nodes.Worker...), 5*time.Minute)

	log.Printf("Installing Master:")
	RunViaSSH(awsos.CommandsToInstallK8sMaster, awsos.AWSUser,
		nodes.Master, 5*time.Minute)

	log.Printf("Installing Worker:")
	RunViaSSH(awsos.CommandsToInstallK8s, awsos.AWSUser,
		nodes.Worker, 5*time.Minute)
}

func PrintNodes(plan *PlanAWS) {
	log.Printf("Created etcd nodes:")
	printNode(&plan.Etcd)
	log.Printf("Created master nodes:")
	printNode(&plan.Master)
	log.Printf("Created worker nodes:")
	printNode(&plan.Worker)
}

func printNode(aws *[]AWSNodeDeets) {
	for _, node := range *aws {
		log.Printf("\t%v (%v, %v)", node.Hostname, node.Publicip, node.Privateip)
	}
}

func FailIfError(err error, message ...string) {
	if err != nil {
		log.Printf(message[0]+": %v\n%v", err, message[1:])
		Fail(message[0])
	}
}

func CopyKismaticToTemp() string {
	tmpDir, err := ioutil.TempDir("", "kisint")
	if err != nil {
		log.Fatal("Error making temp dir: ", err)
	}

	return tmpDir
}

// CopyFile copies the contents of the file named src to the file named
// by dst. The file will be created if it does not already exist. If the
// destination file exists, all it's contents will be replaced by the contents
// of the source file. The file mode will be copied from the source and
// the copied data is synced/flushed to stable storage.
func CopyFile(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return
	}
	defer func() {
		if e := out.Close(); e != nil {
			err = e
		}
	}()

	_, err = io.Copy(out, in)
	if err != nil {
		return
	}

	err = out.Sync()
	if err != nil {
		return
	}

	si, err := os.Stat(src)
	if err != nil {
		return
	}
	err = os.Chmod(dst, si.Mode())
	if err != nil {
		return
	}

	return
}

// CopyDir recursively copies a directory tree, attempting to preserve permissions.
// Source directory must exist, destination directory must *not* exist.
// Symlinks are ignored and skipped.
func CopyDir(src string, dst string) (err error) {
	src = filepath.Clean(src)
	dst = filepath.Clean(dst)

	si, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !si.IsDir() {
		return fmt.Errorf("source is not a directory")
	}
	_, err = os.Stat(dst)
	if err != nil && !os.IsNotExist(err) {
		return
	}
	if err == nil {
		return fmt.Errorf("destination already exists")
	}
	err = os.MkdirAll(dst, si.Mode())
	if err != nil {
		return
	}
	entries, err := ioutil.ReadDir(src)
	if err != nil {
		return
	}

	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())
		if entry.IsDir() {
			err = CopyDir(srcPath, dstPath)
			if err != nil {
				return
			}
		} else {
			// Skip symlinks.
			if entry.Mode()&os.ModeSymlink != 0 {
				continue
			}
			err = CopyFile(srcPath, dstPath)
			if err != nil {
				return
			}
		}
	}
	return
}

func GenerateGUIDString() (string, error) {
	randomness, randomErr := guidMaker.NextId()

	if randomErr != nil {
		return "", randomErr
	}

	return strconv.FormatInt(randomness, 16), nil
}

func AssertKismaticDirectory(kisPath string) {
	if FileExists(kisPath + "/kismatic") {
		log.Fatal("Installer unpacked but kismatic wasn't there")
	}
}

func FileExists(path string) bool {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return false
	}
	return true
}

func MakeETCDNode(nodeType string) (AWSNodeDeets, error) {
	return MakeAWSNode(nodeType, ec2.InstanceTypeT2Micro)
}

func MakeMasterNode(nodeType string) (AWSNodeDeets, error) {
	return MakeAWSNode(nodeType, ec2.InstanceTypeT2Micro)
}

func MakeWorkerNode(nodeType string) (AWSNodeDeets, error) {
	return MakeAWSNode(nodeType, ec2.InstanceTypeT2Medium)
}

func MakeAWSNode(ami string, instanceType string) (AWSNodeDeets, error) {
	svc := ec2.New(session.New(&aws.Config{Region: aws.String(TARGET_REGION)}))
	runResult, err := svc.RunInstances(&ec2.RunInstancesInput{
		ImageId: aws.String(ami),
		BlockDeviceMappings: []*ec2.BlockDeviceMapping{
			{
				DeviceName: aws.String("/dev/sda1"),
				Ebs: &ec2.EbsBlockDevice{
					DeleteOnTermination: aws.Bool(true),
					VolumeSize:          aws.Int64(8),
				},
			},
		},
		InstanceType:     aws.String(instanceType),
		MinCount:         aws.Int64(1),
		MaxCount:         aws.Int64(1),
		SubnetId:         aws.String(SUBNETID),
		KeyName:          aws.String(KEYNAME),
		SecurityGroupIds: []*string{aws.String(SECURITYGROUPID)},
	})

	if err != nil {
		return AWSNodeDeets{}, err
	}

	re := regexp.MustCompile("[^.]*")
	hostname := re.FindString(*runResult.Instances[0].PrivateDnsName)

	deets := AWSNodeDeets{
		Instanceid: *runResult.Instances[0].InstanceId,
		Privateip:  *runResult.Instances[0].PrivateIpAddress,
		Hostname:   hostname,
	}

	for i := 1; i < 4; i++ { //
		params := &ec2.ModifyInstanceAttributeInput{
			InstanceId: aws.String(deets.Instanceid), // Required
			SourceDestCheck: &ec2.AttributeBooleanValue{
				Value: aws.Bool(false),
			},
		}
		svc = ec2.New(session.New(&aws.Config{Region: aws.String(TARGET_REGION)}))
		_, err2 := svc.ModifyInstanceAttribute(params)

		if err2 != nil {
			if i == 3 {
				return AWSNodeDeets{}, err2
			}
			fmt.Printf("Error encountered; retry %v (%v)", i, err2)
			time.Sleep(1 * time.Second)
			continue
		}
		break
	}

	thisHost, _ := os.Hostname()

	svc = ec2.New(session.New(&aws.Config{Region: aws.String(TARGET_REGION)}))
	_, errtag := svc.CreateTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String(deets.Instanceid)},
		Tags: []*ec2.Tag{
			{
				Key:   aws.String("ApprendaTeam"),
				Value: aws.String("Kismatic"),
			},
			{
				Key:   aws.String("CreatedBy"),
				Value: aws.String(thisHost),
			},
		},
	})
	if errtag != nil {
		return deets, errtag
	}

	return deets, nil
}

func TerminateInstances(instanceids ...string) {
	if leaveIt() {
		return
	}
	awsinstanceids := make([]*string, len(instanceids))
	for i, id := range instanceids {
		awsinstanceids[i] = aws.String(id)
	}
	sess, err := session.NewSession()

	if err != nil {
		log.Printf("failed to create session: %v", err)
		return
	}

	svc := ec2.New(sess, &aws.Config{Region: aws.String(TARGET_REGION)})

	params := &ec2.TerminateInstancesInput{
		InstanceIds: awsinstanceids,
	}
	resp, err := svc.TerminateInstances(params)

	if err != nil {
		log.Printf("Could not terminate: %v", resp)
		return
	}
}

func WaitForAllInstancesToStart(plan *PlanAWS) error {
	for i := 0; i < len(plan.Etcd); i++ {
		if err := WaitForInstanceToStart(plan.SSHUser, plan.SSHKeyFile, &plan.Etcd[i]); err != nil {
			return err
		}
	}
	for i := 0; i < len(plan.Master); i++ {
		if err := WaitForInstanceToStart(plan.SSHUser, plan.SSHKeyFile, &plan.Master[i]); err != nil {
			return err
		}
	}
	for i := 0; i < len(plan.Worker); i++ {
		if err := WaitForInstanceToStart(plan.SSHUser, plan.SSHKeyFile, &plan.Worker[i]); err != nil {
			return err
		}
	}

	return nil
}

func WaitForInstanceToStart(sshUser, sshKey string, nodes ...*AWSNodeDeets) error {
	sess, err := session.NewSession()

	if err != nil {
		fmt.Println("failed to create session,", err)
		return err
	}

	fmt.Print("Waiting for nodes to come up")
	defer fmt.Println()

	svc := ec2.New(sess, &aws.Config{Region: aws.String(TARGET_REGION)})
	for _, deets := range nodes {
		deets.Publicip = ""

		for deets.Publicip == "" {
			fmt.Print(".")
			descResult, descErr := svc.DescribeInstances(&ec2.DescribeInstancesInput{
				InstanceIds: []*string{aws.String(deets.Instanceid)},
			})
			if descErr != nil {
				return descErr
			}

			if *descResult.Reservations[0].Instances[0].State.Name == ec2.InstanceStateNameRunning &&
				descResult.Reservations[0].Instances[0].PublicIpAddress != nil {
				deets.Publicip = *descResult.Reservations[0].Instances[0].PublicIpAddress
				BlockUntilSSHOpen(deets, sshUser, sshKey)
			} else {
				time.Sleep(1 * time.Second)
			}
		}
	}
	return nil
}

func BlockUntilSSHOpen(node *AWSNodeDeets, sshUser, sshKey string) {
	for {
		cmd := exec.Command("ssh")
		cmd.Args = append(cmd.Args, "-i", sshKey)
		cmd.Args = append(cmd.Args, "-o", "ConnectTimeout=5")
		cmd.Args = append(cmd.Args, "-o", "BatchMode=yes")
		cmd.Args = append(cmd.Args, "-o", "StrictHostKeyChecking=no")
		cmd.Args = append(cmd.Args, fmt.Sprintf("%s@%s", sshUser, node.Publicip), "exit") // just call exit if we are able to connect
		if err := cmd.Run(); err == nil {
			// command succeeded
			return
		}
		fmt.Printf("?")
		time.Sleep(1 * time.Second)
	}
}

func deployDockerRegistry(node AWSNodeDeets, awsos AWSOSDetails) (hostname string, port int, caPath string) {
	const InstallDockerFromScriptCommand = `sudo curl -sSL https://get.docker.com/ | sh`
	const StartDockerCommand = `sudo systemctl start docker`
	const CreateDockerCertsDir = `mkdir ~/certs`
	const StartDockerRegistryCommand = `sudo docker run -d -p 443:5000 --restart=always --name registry -v ~/certs:/certs -e REGISTRY_HTTP_TLS_CERTIFICATE=/certs/docker.pem -e REGISTRY_HTTP_TLS_KEY=/certs/docker-key.pem registry`
	// Install Docker on etcd
	success := RunViaSSH([]string{InstallDockerFromScriptCommand, StartDockerCommand, CreateDockerCertsDir}, awsos.AWSUser, []AWSNodeDeets{node}, 10*time.Minute)
	if !success {
		Fail("docker install error")
	}

	// Generate CA
	subject := tls.Subject{
		Organization:       "someOrg",
		OrganizationalUnit: "someOrgUnit",
	}
	key, caCert, err := tls.NewCACert("test-tls/ca-csr.json", "someCommonName", subject)

	FailIfError(err, fmt.Sprintf("error creating Docker CA cert: %v", err))

	ioutil.WriteFile("docker-ca.pem", caCert, 0644)

	// Generate certs
	ca := &tls.CA{
		Key:        key,
		Cert:       caCert,
		ConfigFile: "test-tls/ca-config.json",
		Profile:    "kubernetes",
	}
	certHosts := []string{node.Hostname, node.Privateip, node.Publicip}
	req := csr.CertificateRequest{
		CN: node.Hostname,
		KeyRequest: &csr.BasicKeyRequest{
			A: "rsa",
			S: 2048,
		},
		Hosts: certHosts,
		Names: []csr.Name{
			{
				C:  "US",
				L:  "Troy",
				O:  "Kubernetes",
				OU: "Cluster",
				ST: "New York",
			},
		},
	}

	pwd, _ := os.Getwd()

	key, cert, err := tls.NewCert(ca, req)
	FailIfError(err, fmt.Sprintf("error creating Docker certs: %v", err))
	ioutil.WriteFile("docker.pem", cert, 0644)
	ioutil.WriteFile("docker-key.pem", key, 0644)

	CopyFileToRemote(pwd+"/docker.pem", "~/certs/docker.pem", awsos.AWSUser, []AWSNodeDeets{node}, 1*time.Minute)
	CopyFileToRemote(pwd+"/docker-key.pem", "~/certs/docker-key.pem", awsos.AWSUser, []AWSNodeDeets{node}, 1*time.Minute)
	success = RunViaSSH([]string{StartDockerRegistryCommand}, awsos.AWSUser, []AWSNodeDeets{node}, 1*time.Minute)
	if !success {
		Fail("docker registry error")
	}

	return node.Hostname, 443, pwd + "/docker-ca.pem"
}
