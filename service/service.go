package service

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	dockerCli "github.com/docker/docker/api/client"
	apiClient "github.com/fsouza/go-dockerclient"
)

type Service struct {
	Name                 string
	LogPrefix            string
	Env                  interface{} `yaml:"environment"`
	EnvironmentVariables []string
	Expose               string              `yaml:"expose"`
	Image                string              `yaml:"image"`
	BuildDir             string              `yaml:"build"`
	Dns                  []string            `yaml:"dns"`
	NetworkingMode       string              `yaml:"net"`
	Command              string              `yaml:"command"`
	Links                []string            `yaml:"links"`
	Ports                []string            `yaml:"ports"`
	Volumes              map[string]struct{} `yaml:"volumes"`
	VolumesFrom          []string            `yaml:"volumes_from"`
	WorkingDir           string              `yaml:"working_dir"`
	Entrypoint           string              `yaml:"entrypoint"`
	User                 string              `yaml:"user"`
	HostName             string              `yaml:"hostname"`
	DomainName           string              `yaml:"domainname"`
	MemLimit             string              `yaml:"mem_limit"`
	Privileged           bool                `yaml:"privileged"`
	WatchDirs            []string            `yaml:"watch"`

	ExposedPorts map[apiClient.Port]struct{}
	Container    apiClient.Container
	OnAttachHook func(io.Reader, Service)
	Api          *apiClient.Client
	Cli          *dockerCli.DockerCli
}

type EnvDemarshallingError struct{}

func (e EnvDemarshallingError) Error() string {
	return "fig.yml environment setting does not map to slice of strings, or dictionary of string: string"
}

func (s *Service) Build() error {
	err := s.Cli.CmdBuild("-t", s.Image, s.BuildDir)
	return err
}

/**
  This is weird looking but Docker API expects JSON such as :

	 "PortBindings": {
		"80/tcp": [
			{
				"HostIp": "0.0.0.0",
				"HostPort": "49153"
			}
		]
	 },

  	to define port bindings, so this function creates the data structure
	that gets marshalled into that JSON.
*/
func (s *Service) createPortBindings() map[apiClient.Port][]apiClient.PortBinding {
	bindingsToMarshal := make(map[apiClient.Port][]apiClient.PortBinding)
	for _, portBinding := range s.Ports {
		ports := strings.Split(portBinding, ":")
		val := []apiClient.PortBinding{}
		key := apiClient.Port(fmt.Sprintf("%s/tcp", ports[0]))
		if len(ports) > 1 {
			val = append(val, apiClient.PortBinding{
				HostIp:   "0.0.0.0",
				HostPort: ports[1],
			})
		}
		bindingsToMarshal[key] = val
	}
	return bindingsToMarshal
}

func (s *Service) configureExposedPorts() {
	s.ExposedPorts = make(map[apiClient.Port]struct{})
	for _, binding := range s.Ports {
		ports := strings.Split(binding, ":")
		if len(ports) > 1 {
			exposedPortKey := apiClient.Port(fmt.Sprintf("%s/tcp", ports[1]))
			s.ExposedPorts[exposedPortKey] = struct{}{}
		}
	}
}

func envInterfaceToString(i interface{}) (string, error) {
	var vStr string
	switch v := i.(type) {
	case int:
		vStr = fmt.Sprintf("%v", v)
	case float64:
		vStr = fmt.Sprintf("%v", v)
	case string:
		vStr = v
	default:
		// didn't recognize the type
		return "", EnvDemarshallingError{}
	}
	return vStr, nil
}

func (s *Service) createEnvironmentVariables() error {
	if envMap, ok := s.Env.(map[interface{}]interface{}); ok {
		for key, value := range envMap {
			eStrKey, err := envInterfaceToString(key)
			if err != nil {
				return err
			}
			eStrVal, err := envInterfaceToString(value)
			if err != nil {
				return err
			}
			s.EnvironmentVariables = append(s.EnvironmentVariables, fmt.Sprintf("%s=%s", eStrKey, eStrVal))
		}
		return nil
	}
	if envStringSlice, ok := s.Env.([]interface{}); ok {
		for _, e := range envStringSlice {
			eStrVal, err := envInterfaceToString(e)
			if err != nil {
				return err
			}
			s.EnvironmentVariables = append(s.EnvironmentVariables, eStrVal)
		}
		return nil
	}
	return EnvDemarshallingError{}
}

func (s *Service) Create() error {
	s.configureExposedPorts()
	if s.Env != nil {
		err := s.createEnvironmentVariables()
		if err != nil {
			return err
		}
	}
	volumesFrom := ""
	if s.VolumesFrom != nil {
		volumesFrom = s.VolumesFrom[0]
	}

	config := apiClient.Config{
		AttachStdout: true,
		AttachStdin:  false,
		Image:        s.Image,
		Cmd:          strings.Fields(s.Command),
		Env:          s.EnvironmentVariables,
		ExposedPorts: s.ExposedPorts,
		Volumes:      s.Volumes,
		VolumesFrom:  volumesFrom,
	}
	createOpts := apiClient.CreateContainerOptions{
		Name:   s.Name,
		Config: &config,
	}
	container, err := s.Api.CreateContainer(createOpts)
	if err != nil {
		if err == apiClient.ErrNoSuchImage {
			pullOpts := apiClient.PullImageOptions{
				Repository: s.Image,
			}
			fmt.Println("Unable to find image", s.Image, "locally, pulling...")
			err = s.Api.PullImage(pullOpts, apiClient.AuthConfiguration{})
			if err != nil {
				return err
			}
			s.Create()
		}
		return err
	}
	s.Container = *container
	return nil
}

func (s *Service) Start() error {
	links := []string{}
	// TODO: this should work like pyfig
	for _, link := range s.Links {
		links = append(links, fmt.Sprintf("%s:%s_1", link, link))
	}
	err := s.Api.StartContainer(s.Container.ID, &apiClient.HostConfig{
		Links:        links,
		PortBindings: s.createPortBindings(),
		VolumesFrom:  s.VolumesFrom,
	})
	if err != nil {
		return err
	}
	return nil
}

func (s *Service) Restart() error {
	err := s.Api.RestartContainer(s.Name, 10)
	if err != nil {
		return err
	}
	return nil
}
func (s *Service) Stop() error {
	if s.IsRunning() {
		err := s.stop()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error stopping running service!")
			return err
		}
	}
	return nil
}

func (s *Service) stop() error {
	err := s.Api.StopContainer(s.Name, 10)
	if err != nil {
		return err
	}
	return nil
}

func (s *Service) Remove() error {
	if s.Exists() {
		err := s.remove()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error removing running service!")
			return err
		}
	}
	return nil
}

func (s *Service) remove() error {
	err := s.Api.RemoveContainer(apiClient.RemoveContainerOptions{
		ID: s.Name,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "attempt to remove container ", s.Name, "failed", err)
	}
	return nil
}

func (s *Service) Kill() error {
	options := apiClient.KillContainerOptions{
		ID: s.Name,
	}
	err := s.Api.KillContainer(options)
	if err != nil {
		return err
	}
	return nil
}

func (s *Service) IsRunning() bool {
	container, err := s.Api.InspectContainer(s.Name)
	if err != nil {
		if _, ok := err.(*apiClient.NoSuchContainer); !ok {
			fmt.Fprintln(os.Stderr, "non-NoSuchContainer error checking if container is running: ", err)
		}
		return false
	}
	return container.State.Running
}

func (s *Service) Exists() bool {
	_, err := s.Api.InspectContainer(s.Name)
	if err != nil {
		if _, ok := err.(*apiClient.NoSuchContainer); !ok {
			fmt.Fprintln(os.Stderr, "non-NoSuchContainer error checking if container is running: ", err)
		}
		return false
	}
	return true
}

func (s *Service) Wait(wg *sync.WaitGroup) (int, error) {
	defer wg.Done()
	exitCode, err := s.Api.WaitContainer(s.Name)
	if err != nil {
		fmt.Fprintln(os.Stderr, "container wait had error", err)
		return exitCode, err
	}
	return exitCode, nil
}

func defaultOnAttachHook(reader io.Reader, s Service) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		fmt.Printf("%s%s \n", s.LogPrefix, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "There was an error with the scanner in attached container", err)
	}
}

func (s *Service) Attach() error {
	r, w := io.Pipe()
	options := apiClient.AttachToContainerOptions{
		Container:    s.Name,
		OutputStream: w,
		ErrorStream:  w,
		Stream:       true,
		Stdout:       true,
		Stderr:       true,
		Logs:         true,
	}
	fmt.Println("Attaching to container", s.Name)
	go s.Api.AttachToContainer(options)
	if s.OnAttachHook != nil {
		go s.OnAttachHook(r, *s)
	} else {
		go defaultOnAttachHook(r, *s)
	}
	return nil
}
