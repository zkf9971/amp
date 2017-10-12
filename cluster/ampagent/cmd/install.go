package cmd

import (
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"docker.io/go-docker"
	"docker.io/go-docker/api/types"
	"docker.io/go-docker/api/types/filters"
	"docker.io/go-docker/api/types/swarm"
	"github.com/appcelerator/amp/docker/cli/cli/command"
	"github.com/appcelerator/amp/docker/cli/cli/command/stack"
	"github.com/appcelerator/amp/docker/cli/cli/compose/convert"
	"github.com/appcelerator/amp/docker/cli/opts"
	"github.com/appcelerator/amp/docker/docker/pkg/term"
	ampdocker "github.com/appcelerator/amp/pkg/docker"
	"github.com/spf13/cobra"
)

const (
	TARGET_SINGLE  = "single"
	TARGET_CLUSTER = "cluster"
	TEST_NAMESPACE = "test"
)

type InstallOptions struct {
	SkipTests    bool
	NoMonitoring bool
}

var InstallOpts = &InstallOptions{}

func NewInstallCommand() *cobra.Command {
	installCmd := &cobra.Command{
		Use:   "install",
		Short: "Set up amp services in swarm environment",
		RunE:  Install,
	}

	return installCmd
}

func Install(cmd *cobra.Command, args []string) error {
	stdin, stdout, stderr := term.StdStreams()
	dockerCli := ampdocker.NewDockerCli(stdin, stdout, stderr)

	// Create initial secrets
	createInitialSecrets()

	// Create initial configs
	createInitialConfigs()

	// Create initial networks
	createInitialNetworks()

	namespace := "amp"
	if len(args) > 0 && args[0] != "" {
		namespace = args[0]
	}

	// Handle interrupt signal
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	go func() {
		for range c {
			log.Println("\nReceived an interrupt signal - removing AMP services")
			stack.RunRemove(dockerCli, stack.RemoveOptions{Namespaces: []string{namespace, TEST_NAMESPACE}})
			os.Exit(1)
		}
	}()

	etcdClusterMode, err := serviceDeploymentMode(dockerCli.Client(), "amp.type.kv", "true")
	if err != nil {
		return err
	}
	elasticsearchClusterMode, err := serviceDeploymentMode(dockerCli.Client(), "amp.type.search", "true")
	if err != nil {
		return err
	}
	clusterMode := map[string]string{"elasticsearch": elasticsearchClusterMode, "etcd": etcdClusterMode}
	files, err := getStackFiles("./stacks", clusterMode)
	if err != nil {
		return err
	}

	for _, f := range files {
		if strings.Contains(f, ".ampmon.") && InstallOpts.NoMonitoring {
			continue
		}
		if strings.Contains(f, ".test.") {
			continue
		}
		//	err := deployTest(dockerCli, f, "test", 60 /* timeout in seconds */)
		//	stack.RunRemove(dockerCli, stack.RemoveOptions{Namespaces: []string{TEST_NAMESPACE}})
		//	if err != nil {
		//		stack.RunRemove(dockerCli, stack.RemoveOptions{Namespaces: []string{namespace}})
		//		return err
		//	}
		log.Println(f)
		err := deploy(dockerCli, f, namespace)
		if err != nil {
			stack.RunRemove(dockerCli, stack.RemoveOptions{Namespaces: []string{namespace, TEST_NAMESPACE}})
			return err
		}
	}
	return nil
}

// returns the deployment mode
// based on the number of nodes with the label passed as argument
// if number of nodes > 2, mode = cluster, else mode = single
func serviceDeploymentMode(c docker.APIClient, labelKey string, labelValue string) (string, error) {
	// unfortunately filtering labels on NodeList won't work as expected, Cf. https://github.com/moby/moby/issues/27231
	nodes, err := c.NodeList(context.Background(), types.NodeListOptions{})
	if err != nil {
		return "", err
	}
	matchingNodes := 0
	for _, node := range nodes {
		// node is a swarm.Node
		for k, v := range node.Spec.Labels {
			if k == labelKey {
				if labelValue == "" || labelValue == v {
					matchingNodes++
				}
			}
		}
	}
	switch matchingNodes {
	case 0:
		return "", fmt.Errorf("can't find a node with label %s", labelKey)
	case 1:
		fallthrough
	case 2:
		return TARGET_SINGLE, nil
	default:
		return TARGET_CLUSTER, nil
	}
}

// returns sorted list of yaml file pathnames
func getStackFiles(path string, clusterMode map[string]string) ([]string, error) {
	if path == "" {
		path = "./stacks"
	}

	// a bit more work but we can't just use filepath.Glob
	// since we need to match both *.yml and *.yaml
	files, err := ioutil.ReadDir(path)
	if err != nil {
		return nil, err
	}
	stackfiles := []string{}
	companionFilesToSkip := map[string]bool{}
	for _, f := range files {
		name := f.Name()
		// not compiling regex since only expecting less than a dozen stackfiles
		matched, err := regexp.MatchString("\\.ya?ml$", name)
		if err != nil {
			log.Println(err)
		} else if matched {
			if strings.Contains(name, ".ampmon.") && InstallOpts.NoMonitoring {
				// we skip this file, and we'll skip also the companion test file
				if split := strings.Split(name, "."); len(split) == 3 {
					companionFilesToSkip[fmt.Sprintf("%s.test.yml", split[0])] = true
				}
				continue
			}
			if strings.Contains(name, ".test.") && InstallOpts.SkipTests {
				continue
			}
			if _, skip := companionFilesToSkip[name]; skip {
				continue
			}
			// looking for the service name, in case there's an indication for the cluster mode (single vs cluster)
			// expecting a file with a name NN-SERVICENAME-mode.*
			split := strings.Split(name, "-")
			if len(split) == 3 {
				serviceName := split[1]
				if strings.Contains(name, TARGET_SINGLE) && clusterMode[serviceName] != TARGET_SINGLE {
					continue
				}
				if strings.Contains(name, TARGET_CLUSTER) && clusterMode[serviceName] != TARGET_CLUSTER {
					continue
				}
			}
			stackfiles = append(stackfiles, filepath.Join(path, name))
		}
	}
	return stackfiles, nil
}

func deploy(d *command.DockerCli, stackfile string, namespace string) error {
	return deployExpectingState(d, stackfile, namespace, swarm.TaskStateRunning)
}

func deployExpectingState(d *command.DockerCli, stackfile string, namespace string, expectedState swarm.TaskState) error {
	if namespace == "" {
		// use the stackfile basename as the default stack namespace
		namespace = filepath.Base(stackfile)
		namespace = strings.TrimSuffix(namespace, filepath.Ext(namespace))
	}

	opts := stack.DeployOptions{
		Namespace:        namespace,
		Composefile:      stackfile,
		ResolveImage:     stack.ResolveImageAlways,
		SendRegistryAuth: false,
		Prune:            false,
	}

	err := stack.RunDeploy(d, opts)
	if err == nil {
		log.Printf("Service has reached expected state (%s)\n", string(expectedState))
	}

	// Create a docker client
	docker := ampdocker.NewClient(ampdocker.DefaultURL, ampdocker.DefaultVersion)
	if err := docker.Connect(); err != nil {
		return err
	}
	errors := docker.WaitOnStack(context.Background(), namespace, os.Stdout)
	for _, err := range errors {
		if err != nil {
			return err
		}
	}
	return nil
}

func deployTest(d *command.DockerCli, stackfile string, namespace string, timeout int) error {
	// Deploy the test stack
	if err := deployExpectingState(d, stackfile, namespace, swarm.TaskStateComplete); err != nil {
		return err
	}

	// Create a docker client
	c, err := docker.NewClient(ampdocker.DefaultURL, ampdocker.DefaultVersion, nil, nil)
	if err != nil {
		return err
	}

	// List stack tasks
	options := types.TaskListOptions{Filters: filters.NewArgs()}
	options.Filters.Add("label", convert.LabelNamespace+"="+namespace)
	tasks, err := c.TaskList(context.Background(), options)
	if err != nil {
		return err
	}

	// Assert we have at least one task
	if len(tasks) == 0 {
		return fmt.Errorf("no task for test")
	}

	// Assert we have only one task
	if len(tasks) != 1 {
		return fmt.Errorf("too many tasks for test: %d", len(tasks))
	}

	// If the task has an error, the test has failed
	task := tasks[0]
	if task.Status.Err != "" {
		return fmt.Errorf("test failed with status: %s", task.Status.Err)
	}

	log.Println("Test successful")
	return nil
}

func ListSecrets() ([]swarm.Secret, error) {
	c, err := docker.NewClient(ampdocker.DefaultURL, ampdocker.DefaultVersion, nil, nil)
	if err != nil {
		return nil, err
	}
	return c.SecretList(context.Background(), types.SecretListOptions{})
}

func SecretExists(name string) (bool, error) {
	secrets, err := ListSecrets()
	if err != nil {
		return false, err
	}
	for _, secret := range secrets {
		if secret.Spec.Name == name {
			return true, nil
		}
	}
	return false, nil
}

func CreateSecret(name string, data []byte) error {
	c, err := docker.NewClient(ampdocker.DefaultURL, ampdocker.DefaultVersion, nil, nil)
	if err != nil {
		return err
	}
	spec := swarm.SecretSpec{
		Annotations: swarm.Annotations{
			Name: name,
		},
		Data: data,
	}
	_, err = c.SecretCreate(context.Background(), spec)
	if err != nil {
		return err
	}
	return nil
}

// AMP secrets map: Secret name paired to secret file in ./defaults
var ampSecrets = map[string]string{
	"alertmanager_yml": "alertmanager.yml",
	"amplifier_yml":    "amplifier.yml",
	"certificate_amp":  "certificate.amp",
}

// This is the default secrets path
const defaultSecretsPath = "defaults"

// exists returns whether the given file or directory exists or not
func pathExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return true, err
}

func createInitialSecrets() error {
	// Computing secret path
	secretPath := path.Join("/", defaultSecretsPath)
	pe, err := pathExists(secretPath)
	if err != nil {
		return err
	}
	if !pe {
		secretPath = defaultSecretsPath
	}
	secretPath, err = filepath.Abs(secretPath)
	if err != nil {
		return err
	}
	log.Println("Using the following path for secrets:", secretPath)

	// Creating secrets
	for secret, filename := range ampSecrets {
		// Check if secret already exists
		exists, err := SecretExists(secret)
		if err != nil {
			return err
		}
		if exists {
			log.Println("Skipping already existing secret:", secret)
			continue
		}

		// Load secret data
		data, err := ioutil.ReadFile(path.Join(secretPath, filename))
		if err != nil {
			return err
		}

		// Create secret
		if err := CreateSecret(secret, data); err != nil {
			return err
		}
		log.Println("Successfully created secret:", secret)
	}
	return nil
}

func ListNetworks() ([]types.NetworkResource, error) {
	c, err := docker.NewClient(ampdocker.DefaultURL, ampdocker.DefaultVersion, nil, nil)
	if err != nil {
		return nil, err
	}
	return c.NetworkList(context.Background(), types.NetworkListOptions{})
}

func NetworkExists(name string) (bool, error) {
	networks, err := ListNetworks()
	if err != nil {
		return false, err
	}
	for _, network := range networks {
		if network.Name == name {
			return true, nil
		}
	}
	return false, nil
}

func CreateNetwork(name string, overlay bool, attachable bool) error {
	c, err := docker.NewClient(ampdocker.DefaultURL, ampdocker.DefaultVersion, nil, nil)
	if err != nil {
		return err
	}
	spec := types.NetworkCreate{
		CheckDuplicate: true,
		Attachable:     attachable,
	}
	if overlay {
		spec.Driver = "overlay"
	}
	_, err = c.NetworkCreate(context.Background(), name, spec)
	if err != nil {
		return err
	}
	return nil
}

func RemoveNetwork(name string) error {
	c, err := docker.NewClient(ampdocker.DefaultURL, ampdocker.DefaultVersion, nil, nil)
	if err != nil {
		return err
	}
	if err := c.NetworkRemove(context.Background(), name); err != nil {
		return err
	}
	return nil
}

const ampnet = "ampnet"

func createInitialNetworks() error {
	// Check if network already exists
	exists, err := NetworkExists(ampnet)
	if err != nil {
		return err
	}
	if exists {
		log.Println("Skipping already existing network:", ampnet)
		return nil
	}

	// Create network
	if err := CreateNetwork(ampnet, true, true); err != nil {
		return err
	}
	log.Println("Successfully created network:", ampnet)
	return nil
}

func removeInitialNetworks() error {
	// Check if network already exists
	exists, err := NetworkExists(ampnet)
	if err != nil {
		return err
	}
	if !exists {
		log.Println("Skipping non existing network:", ampnet)
		return nil
	}

	// Remove network
	time.Sleep(2 * time.Second)
	if err := RemoveNetwork(ampnet); err != nil {
		return err
	}
	log.Println("Successfully removed network:", ampnet)
	return nil
}

func ListConfigs() ([]swarm.Config, error) {
	c, err := docker.NewClient(ampdocker.DefaultURL, ampdocker.DefaultVersion, nil, nil)
	if err != nil {
		return nil, err
	}
	return c.ConfigList(context.Background(), types.ConfigListOptions{})
}

func ConfigExists(name string) (bool, error) {
	configs, err := ListConfigs()
	if err != nil {
		return false, err
	}
	for _, config := range configs {
		if config.Spec.Name == name {
			return true, nil
		}
	}
	return false, nil
}

func CreateConfig(name string, data []byte) error {
	c, err := docker.NewClient(ampdocker.DefaultURL, ampdocker.DefaultVersion, nil, nil)
	if err != nil {
		return err
	}
	spec := swarm.ConfigSpec{
		Annotations: swarm.Annotations{
			Name: name,
		},
		Data: data,
	}
	_, err = c.ConfigCreate(context.Background(), spec)
	if err != nil {
		return err
	}
	return nil
}

// AMP configs map: Config name paired to config file in ./defaults
var ampConfigs = map[string]string{
	"prometheus_alerts_rules": "prometheus_alerts.rules",
}

// This is the default configs path
const defaultConfigsPath = "defaults"

func createInitialConfigs() error {
	// Computing config path
	configPath := path.Join("/", defaultConfigsPath)
	pe, err := pathExists(configPath)
	if err != nil {
		return err
	}
	if !pe {
		configPath = defaultConfigsPath
	}
	configPath, err = filepath.Abs(configPath)
	if err != nil {
		return err
	}
	log.Println("Using the following path for configs:", configPath)

	// Creating configs
	for config, filename := range ampConfigs {
		// Check if config already exists
		exists, err := ConfigExists(config)
		if err != nil {
			return err
		}
		if exists {
			log.Println("Skipping already existing config:", config)
			continue
		}

		// Load config data
		data, err := ioutil.ReadFile(path.Join(configPath, filename))
		if err != nil {
			return err
		}

		// Create config
		if err := CreateConfig(config, data); err != nil {
			return err
		}
		log.Println("Successfully created config:", config)
	}
	return nil
}

func ListVolumes(filter opts.FilterOpt) ([]*types.Volume, error) {
	c, err := docker.NewClient(ampdocker.DefaultURL, ampdocker.DefaultVersion, nil, nil)
	if err != nil {
		return nil, err
	}
	reply, err := c.VolumeList(context.Background(), filter.Value())
	if err != nil {
		return nil, err
	}
	return reply.Volumes, nil
}

func RemoveVolume(name string, force bool, retries int) error {
	c, err := docker.NewClient(ampdocker.DefaultURL, ampdocker.DefaultVersion, nil, nil)
	if err != nil {
		return err
	}

	success := false
	for i := 0; i < 10; i++ {
		if err := c.VolumeRemove(context.Background(), name, force); err != nil {
			time.Sleep(time.Second)
			continue
		}
		success = true
		break
	}
	if !success {
		return fmt.Errorf("Timed out trying to remove volume: %s", name)
	}
	return nil
}

const ampVolumesPrefix = "amp_"

func removeVolumes() error {
	// List amp volumes
	filter := opts.NewFilterOpt()
	filter.Set("name=" + ampVolumesPrefix)
	volumes, err := ListVolumes(filter)
	if err != nil {
		return nil
	}

	// Remove volumes
	for _, volume := range volumes {
		if err := RemoveVolume(volume.Name, true, 10); err != nil {
			return err
		}
		log.Println("Successfully removed volume:", volume.Name)
	}

	return nil
}