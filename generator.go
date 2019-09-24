package dockergen

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	docker "github.com/fsouza/go-dockerclient"
)

type generator struct {
	Client                     *docker.Client
	Configs                    ConfigFile
	Endpoint                   string
	TLSVerify                  bool
	TLSCert, TLSCaCert, TLSKey string
	All                        bool

	wg    sync.WaitGroup
	retry bool
}

// GeneratorConfig holds the configuration of the generator
type GeneratorConfig struct {
	Endpoint string

	TLSCert   string
	TLSKey    string
	TLSCACert string
	TLSVerify bool
	All       bool

	ConfigFile ConfigFile
}

// NewGenerator creates a new generator instance
func NewGenerator(gc GeneratorConfig) (*generator, error) {
	endpoint, err := GetEndpoint(gc.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("Bad endpoint: %s", err)
	}

	client, err := NewDockerClient(endpoint, gc.TLSVerify, gc.TLSCert, gc.TLSCACert, gc.TLSKey)
	if err != nil {
		return nil, fmt.Errorf("Unable to create docker client: %s", err)
	}

	apiVersion, err := client.Version()
	if err != nil {
		log.Printf("Error retrieving docker server version info: %s\n", err)
	}

	// Grab the docker daemon info once and hold onto it
	setDockerEnv(apiVersion)

	return &generator{
		Client:    client,
		Endpoint:  gc.Endpoint,
		TLSVerify: gc.TLSVerify,
		TLSCert:   gc.TLSCert,
		TLSCaCert: gc.TLSCACert,
		TLSKey:    gc.TLSKey,
		All:       gc.All,
		Configs:   gc.ConfigFile,
		retry:     true,
	}, nil
}

func (g *generator) Generate() error {
	g.generateFromContainers()
	g.generateAtInterval()
	g.generateFromEvents()
	g.generateFromSignals()
	g.wg.Wait()

	return nil
}

func (g *generator) generateFromSignals() {
	var hasWatcher bool
	for _, config := range g.Configs.Config {
		if config.Watch {
			hasWatcher = true
			break
		}
	}

	// If none of the configs need to watch for events, don't watch for signals either
	if !hasWatcher {
		return
	}

	g.wg.Add(1)
	go func() {
		defer g.wg.Done()

		sigChan := newSignalChannel()
		for {
			sig := <-sigChan
			log.Printf("Received signal: %s\n", sig)
			switch sig {
			case syscall.SIGHUP:
				g.generateFromContainers()
			case syscall.SIGQUIT, syscall.SIGKILL, syscall.SIGTERM, syscall.SIGINT:
				// exit when context is done
				return
			}
		}
	}()
}

// generateAndNotify generates file and calls notifiers
// if forceNotify is true, notifiers will be called even if the generated file was not changed
func (g *generator) generateAndNotify(config Config, containers Context, forceNotify bool) {
	changed := GenerateFile(config, containers)
	if !changed && !forceNotify {
		log.Printf("Contents of %s did not change. Skipping notifications.", config.Dest)
		return
	} else if !changed {
		log.Printf("Contents of %s did not change, calling notifiers anyway.", config.Dest)
	}

	g.runNotifyCmd(config)
	g.sendSignalToContainer(config)
	g.sendSignalToService(config)
}

func (g *generator) generateFromContainers() {
	containers, err := g.GenerateContext()
	if err != nil {
		log.Printf("Error listing containers: %s\n", err)
		return
	}
	for _, config := range g.Configs.Config {
		g.generateAndNotify(config, containers, false)
	}
}

func (g *generator) generateAtInterval() {
	for _, config := range g.Configs.Config {

		if config.Interval == 0 {
			continue
		}

		log.Printf("Generating every %d seconds", config.Interval)
		g.wg.Add(1)
		ticker := time.NewTicker(time.Duration(config.Interval) * time.Second)
		go func(config Config) {
			defer g.wg.Done()

			sigChan := newSignalChannel()
			for {
				select {
				case <-ticker.C:
					containers, err := g.GenerateContext()
					if err != nil {
						log.Printf("Error listing containers: %s\n", err)
						continue
					}
					g.generateAndNotify(config, containers, true)
				case sig := <-sigChan:
					log.Printf("Received signal: %s\n", sig)
					switch sig {
					case syscall.SIGQUIT, syscall.SIGKILL, syscall.SIGTERM, syscall.SIGINT:
						ticker.Stop()
						return
					}
				}
			}
		}(config)
	}
}

func (g *generator) generateFromEvents() {
	configs := g.Configs.FilterWatches()
	if len(configs.Config) == 0 {
		return
	}

	client := g.Client
	var watchers []chan *docker.APIEvents

	for _, config := range configs.Config {

		if !config.Watch {
			continue
		}

		g.wg.Add(1)

		go func(config Config, watcher chan *docker.APIEvents) {
			defer g.wg.Done()
			watchers = append(watchers, watcher)

			debouncedChan := newDebounceChannel(watcher, config.Wait)
			for range debouncedChan {
				containers, err := g.GenerateContext()
				if err != nil {
					log.Printf("Error listing containers: %s\n", err)
					continue
				}
				g.generateAndNotify(config, containers, false)
			}
		}(config, make(chan *docker.APIEvents, 100))
	}

	// maintains docker client connection and passes events to watchers
	go func() {
		// channel will be closed by go-dockerclient
		eventChan := make(chan *docker.APIEvents, 100)
		sigChan := newSignalChannel()

		for {
			watching := false

			if client == nil {
				var err error
				endpoint, err := GetEndpoint(g.Endpoint)
				if err != nil {
					log.Printf("Bad endpoint: %s", err)
					time.Sleep(10 * time.Second)
					continue
				}
				client, err = NewDockerClient(endpoint, g.TLSVerify, g.TLSCert, g.TLSCaCert, g.TLSKey)
				if err != nil {
					log.Printf("Unable to connect to docker daemon: %s", err)
					time.Sleep(10 * time.Second)
					continue
				}
			}

			for {
				if client == nil {
					break
				}
				if !watching {
					err := client.AddEventListener(eventChan)
					if err != nil && err != docker.ErrListenerAlreadyExists {
						log.Printf("Error registering docker event listener: %s", err)
						time.Sleep(10 * time.Second)
						continue
					}
					watching = true
					log.Println("Watching docker events")
					// sync all configs after resuming listener
					g.generateFromContainers()
				}
				select {
				case event, ok := <-eventChan:
					if !ok {
						log.Printf("Docker daemon connection interrupted")
						if watching {
							client.RemoveEventListener(eventChan)
							watching = false
							client = nil
						}
						if !g.retry {
							// close all watchers and exit
							for _, watcher := range watchers {
								close(watcher)
							}
							return
						}
						// recreate channel and attempt to resume
						eventChan = make(chan *docker.APIEvents, 100)
						time.Sleep(10 * time.Second)
						break
					}
					if event.Status == "start" || event.Status == "stop" || event.Status == "die" {
						log.Printf("Received event %s for container %s", event.Status, shortIdent(event.ID))
						// fanout event to all watchers
						for _, watcher := range watchers {
							watcher <- event
						}
					} else if event.Status == "service:update" {
						log.Printf("Received event %s for service %s", event.Status, shortIdent(event.ID))
						// fanout event to all watchers
						for _, watcher := range watchers {
							watcher <- event
						}
					}
				case <-time.After(10 * time.Second):
					// check for docker liveness
					err := client.Ping()
					if err != nil {
						log.Printf("Unable to ping docker daemon: %s", err)
						if watching {
							client.RemoveEventListener(eventChan)
							watching = false
							client = nil
						}
					}
				case sig := <-sigChan:
					log.Printf("Received signal: %s\n", sig)
					switch sig {
					case syscall.SIGQUIT, syscall.SIGKILL, syscall.SIGTERM, syscall.SIGINT:
						// close all watchers and exit
						for _, watcher := range watchers {
							close(watcher)
						}
						return
					}
				}
			}
		}
	}()
}

func (g *generator) runNotifyCmd(config Config) {
	if config.NotifyCmd == "" {
		return
	}

	log.Printf("Running '%s'", config.NotifyCmd)
	cmd := exec.Command("/bin/sh", "-c", config.NotifyCmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("Error running notify command: %s, %s\n", config.NotifyCmd, err)
	}
	if config.NotifyOutput {
		for _, line := range strings.Split(string(out), "\n") {
			if line != "" {
				log.Printf("[%s]: %s", config.NotifyCmd, line)
			}
		}
	}
}

func (g *generator) sendSignalToContainer(config Config) {
	if len(config.NotifyContainers) < 1 {
		return
	}

	for container, signal := range config.NotifyContainers {
		log.Printf("Sending container '%s' signal '%v'", container, signal)
		killOpts := docker.KillContainerOptions{
			ID:     container,
			Signal: signal,
		}
		if err := g.Client.KillContainer(killOpts); err != nil {
			log.Printf("Error sending signal to container: %s", err)
		}
	}
}

func (g *generator) sendSignalToService(config Config) {
	if len(config.NotifyServices) < 1 {
		return
	}

	for service, signal := range config.NotifyServices {
		log.Printf("Service '%s' needs notification", service)
		taskOpts := docker.ListTasksOptions{
			Filters: map[string][]string{
				"service": []string{service},
			},
		}
		tasks, err := g.Client.ListTasks(taskOpts)
		if err != nil {
			log.Printf("Error retrieving task list: %s", err)
		}
		for _, task := range tasks {
			if task.Status.State != "running" {
				continue
			}

			container := task.Status.ContainerStatus.ContainerID

			log.Printf("Sending container '%s' signal '%v'", shortIdent(container), signal)
			killOpts := docker.KillContainerOptions{
				ID:     container,
				Signal: signal,
			}
			if err := g.Client.KillContainer(killOpts); err != nil {
				log.Printf("Error sending signal to container %s: %s", container, err)
			}
		}
	}
}

func (g *generator) refreshServerInfo() {
	apiInfo, err := g.Client.Info()
	if err != nil {
		log.Printf("Error retrieving docker server info: %s\n", err)
	} else {
		setServerInfo(apiInfo)
	}
}

func (g *generator) getServices() (Services, error) {
	var err error
	svcmap := make(Services)

	g.refreshServerInfo()

	svcs, err := g.Client.ListServices(docker.ListServicesOptions{})
	if err != nil {
		return nil, err
	}

	for _, svc := range svcs {
		s := &Service{
			ID:     svc.ID,
			Name:   svc.Spec.Name,
			Labels: svc.Spec.Labels,
		}

		for _, vip := range svc.Endpoint.VirtualIPs {
			network, err := g.Client.NetworkInfo(vip.NetworkID)
			if err != nil {
				return nil, fmt.Errorf("error inspecting swarm service VIP network %s: %s", vip.NetworkID, err)
			}

			cleanVIP := strings.Split(vip.Addr, "/")[0]
			svcVIPNet := ServiceNetwork{
				IP:     cleanVIP,
				Name:   network.Name,
				Scope:  network.Scope,
				Driver: network.Driver,
			}
			s.Networks = append(s.Networks, svcVIPNet)
		}

		svcmap[svc.ID] = s
	}

	return svcmap, err
}

// GenerateContext generates the context used for template generation
func (g *generator) GenerateContext() (Context, error) {
	// client info
	apiInfo, err := g.Client.Info()
	if err != nil {
		log.Printf("Error retrieving Docker server info: %s\n", err)
	} else {
		setServerInfo(apiInfo)
	}

	// swarm services
	svcs, err := g.getServices()
	if err != nil {
		log.Printf("Error retrieving Docker services: %s\n", err)
	} else {
		setServices(svcs)
	}

	// containers
	apiContainers, err := g.Client.ListContainers(docker.ListContainersOptions{
		All:  g.All,
		Size: false,
	})
	if err != nil {
		return nil, err
	}

	containers := []*RuntimeContainer{}
	for _, apiContainer := range apiContainers {
		container, err := g.Client.InspectContainer(apiContainer.ID)
		if err != nil {
			log.Printf("Error inspecting container: %s: %s\n", apiContainer.ID, err)
			continue
		}

		labels := container.Config.Labels

		registry, repository, tag := splitDockerImage(container.Config.Image)
		runtimeContainer := &RuntimeContainer{
			ID: container.ID,
			Image: DockerImage{
				Registry:   registry,
				Repository: repository,
				Tag:        tag,
			},
			State: State{
				Running: container.State.Running,
			},
			Name:         strings.TrimLeft(container.Name, "/"),
			Hostname:     container.Config.Hostname,
			Gateway:      container.NetworkSettings.Gateway,
			Addresses:    []Address{},
			Networks:     []Network{},
			Env:          make(map[string]string),
			Volumes:      make(map[string]Volume),
			Node:         Node{},
			Labels:       make(map[string]string),
			IP:           container.NetworkSettings.IPAddress,
			IP6LinkLocal: container.NetworkSettings.LinkLocalIPv6Address,
			IP6Global:    container.NetworkSettings.GlobalIPv6Address,
		}
		for k, v := range container.NetworkSettings.Ports {
			address := Address{
				IP:           container.NetworkSettings.IPAddress,
				IP6LinkLocal: container.NetworkSettings.LinkLocalIPv6Address,
				IP6Global:    container.NetworkSettings.GlobalIPv6Address,
				Port:         k.Port(),
				Proto:        k.Proto(),
			}
			if len(v) > 0 {
				address.HostPort = v[0].HostPort
				address.HostIP = v[0].HostIP
			}
			runtimeContainer.Addresses = append(runtimeContainer.Addresses,
				address)

		}
		for k, v := range container.NetworkSettings.Networks {
			network := Network{
				IP:                  v.IPAddress,
				Name:                k,
				Gateway:             v.Gateway,
				EndpointID:          v.EndpointID,
				IPv6Gateway:         v.IPv6Gateway,
				GlobalIPv6Address:   v.GlobalIPv6Address,
				MacAddress:          v.MacAddress,
				GlobalIPv6PrefixLen: v.GlobalIPv6PrefixLen,
				IPPrefixLen:         v.IPPrefixLen,
			}

			runtimeContainer.Networks = append(runtimeContainer.Networks,
				network)
		}
		for k, v := range container.Volumes {
			runtimeContainer.Volumes[k] = Volume{
				Path:      k,
				HostPath:  v,
				ReadWrite: container.VolumesRW[k],
			}
		}

		// Swarm node
		if container.Node != nil {
			runtimeContainer.Node.ID = container.Node.ID
			runtimeContainer.Node.Name = container.Node.Name
			runtimeContainer.Node.Address = Address{
				IP: container.Node.IP,
			}
		} else {
			if nodeID, ok := labels["com.docker.swarm.node.id"]; ok {
				node, err := g.Client.InspectNode(nodeID)
				if err != nil {
					log.Printf("Error inspecting swarm node %s: %s\n", shortIdent(nodeID), err)
				} else {
					runtimeContainer.Node = Node{
						ID:   node.ID,
						Name: node.Spec.Name,
						Address: Address{
							IP: node.Status.Addr,
						},
					}
				}
			}
		}

		// Swarm service
		if serviceID, ok := labels["com.docker.swarm.service.id"]; ok {
			svc, has := svcs[serviceID]
			if !has {
				log.Printf("Container %s is belonging to a non-existent service %s\n",
					shortIdent(container.ID), shortIdent(serviceID))
			} else {
				runtimeContainer.Service = svc
			}
		}

		for _, v := range container.Mounts {
			runtimeContainer.Mounts = append(runtimeContainer.Mounts, Mount{
				Name:        v.Name,
				Source:      v.Source,
				Destination: v.Destination,
				Driver:      v.Driver,
				Mode:        v.Mode,
				RW:          v.RW,
			})
		}

		runtimeContainer.Env = splitKeyValueSlice(container.Config.Env)
		runtimeContainer.Labels = container.Config.Labels
		containers = append(containers, runtimeContainer)
	}
	return containers, nil

}

func newSignalChannel() <-chan os.Signal {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGKILL)

	return sig
}

func newDebounceChannel(input chan *docker.APIEvents, wait *Wait) chan *docker.APIEvents {
	if wait == nil {
		return input
	}
	if wait.Min == 0 {
		return input
	}

	output := make(chan *docker.APIEvents, 100)

	go func() {
		var (
			event    *docker.APIEvents
			minTimer <-chan time.Time
			maxTimer <-chan time.Time
		)

		defer close(output)

		for {
			select {
			case buffer, ok := <-input:
				if !ok {
					return
				}
				event = buffer
				minTimer = time.After(wait.Min)
				if maxTimer == nil {
					maxTimer = time.After(wait.Max)
				}
			case <-minTimer:
				log.Println("Debounce minTimer fired")
				minTimer, maxTimer = nil, nil
				output <- event
			case <-maxTimer:
				log.Println("Debounce maxTimer fired")
				minTimer, maxTimer = nil, nil
				output <- event
			}
		}
	}()

	return output
}
