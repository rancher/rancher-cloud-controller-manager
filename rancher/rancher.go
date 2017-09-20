package rancher

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/golang/glog"
	"github.com/rancher/go-rancher/v3"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	api "k8s.io/kubernetes/pkg/api/v1"
	"k8s.io/kubernetes/pkg/cloudprovider"
	"k8s.io/kubernetes/pkg/controller"
)

type Host struct {
	RancherHost *client.Host
}

type PublicEndpoint struct {
	IPAddress string
	Port      int
}

const (
	providerName                = "rancher"
	lbNameFormat         string = "lb-%s"
	kubernetesEnvName    string = "kubernetes-loadbalancers"
	kubernetesExternalId string = "kubernetes-loadbalancers://"
)

var allowedChars = regexp.MustCompile("[^a-zA-Z0-9-]")
var dupeHyphen = regexp.MustCompile("-+")

// CloudProvider implents Instances, Zones, and LoadBalancer
type CloudProvider struct {
	client    *client.RancherClient
	conf      *rConfig
	hostCache cache.Store
}

// Initialize passes a Kubernetes clientBuilder interface to the cloud provider
func (r *CloudProvider) Initialize(clientBuilder controller.ControllerClientBuilder) {}

// ProviderName returns the cloud provider ID.
func (r *CloudProvider) ProviderName() string {
	return providerName
}

// ScrubDNS filters DNS settings for pods.
func (r *CloudProvider) ScrubDNS(nameservers, searches []string) (nsOut, srchOut []string) {
	return nameservers, searches
}

// LoadBalancer returns an implementation of LoadBalancer for Rancher
func (r *CloudProvider) LoadBalancer() (cloudprovider.LoadBalancer, bool) {
	return r, true
}

// Zones returns an implementation of Zones for Rancher
func (r *CloudProvider) Zones() (cloudprovider.Zones, bool) {
	return r, true
}

// Instances returns an implementation of Instances for Rancher
func (r *CloudProvider) Instances() (cloudprovider.Instances, bool) {
	return r, true
}

// Clusters not supported
func (r *CloudProvider) Clusters() (cloudprovider.Clusters, bool) {
	return nil, false
}

// Routes not supported
func (r *CloudProvider) Routes() (cloudprovider.Routes, bool) {
	return nil, false
}

// --- LoadBalancer Functions ---

type instanceCollection struct {
	Data []instanceAndHost `json:"data,omitempty"`
}

type instanceAndHost struct {
	client.Instance
	Hosts []client.Host `json:"hosts,omitempty"`
}

// GetLoadBalancer is an implementation of LoadBalancer.GetLoadBalancer
func (r *CloudProvider) GetLoadBalancer(clusterName string, service *api.Service) (status *api.LoadBalancerStatus, exists bool, retErr error) {
	name := formatLBName(cloudprovider.GetLoadBalancerName(service))
	glog.Infof("GetLoadBalancer [%s]", name)

	lb, err := r.getLBByName(name)
	if err != nil {
		return nil, false, err
	}

	if lb == nil {
		glog.Infof("Can't find lb by name [%s]", name)
		return &api.LoadBalancerStatus{}, false, nil
	}

	return r.toLBStatus(lb)
}

// EnsureLoadBalancer is an implementation of LoadBalancer.EnsureLoadBalancer.
func (r *CloudProvider) EnsureLoadBalancer(clusterName string, service *api.Service, nodes []*api.Node) (*api.LoadBalancerStatus, error) {
	hosts := []string{}

	for _, node := range nodes {
		hosts = append(hosts, node.Name)
	}

	name := formatLBName(cloudprovider.GetLoadBalancerName(service))
	loadBalancerIP := service.Spec.LoadBalancerIP
	ports := service.Spec.Ports
	affinity := service.Spec.SessionAffinity
	glog.Infof("EnsureLoadBalancer [%s] [%#v] [%#v] [%s] [%s]", name, loadBalancerIP, ports, hosts, affinity)

	if loadBalancerIP != "" {
		// Rancher doesn't support specifying loadBalancer IP
		return nil, fmt.Errorf("loadBalancerIP cannot be specified for Rancher LoadBalancer")
	}

	if affinity != api.ServiceAffinityNone {
		// Rancher supports sticky sessions, but only when configured for HTTP/HTTPS
		return nil, fmt.Errorf("Unsupported load balancer affinity: %v", affinity)
	}

	lb, err := r.getLBByName(name)
	if err != nil {
		return nil, err
	}

	lbPorts := []string{}
	for _, port := range ports {
		if port.NodePort == 0 {
			glog.Warningf("Ignoring port without NodePort: %s", port)
		}
		lbPorts = append(lbPorts, fmt.Sprintf("%v:%v/tcp", port.Port, port.Port))
	}

	if lb != nil && portsChanged(lbPorts, lb.LaunchConfig.Ports) {
		glog.Infof("Deleting the lb because the ports changed %s", lb.Name)
		// Cannot update ports on an LB, so if the ports have changed, need to recreate
		err = r.deleteLoadBalancer(lb)
		if err != nil {
			return nil, err
		}
		lb = nil
	}

	var imageUUID string
	imageUUID, fetched := r.GetSetting("lb.instance.image")
	if !fetched || imageUUID == "" {
		return nil, fmt.Errorf("Failed to fetch lb.instance.image setting")
	}
	imageUUID = fmt.Sprintf("docker:%s", imageUUID)

	if lb == nil {
		env, err := r.getOrCreateEnvironment()
		if err != nil {
			return nil, err
		}

		lb = &client.LoadBalancerService{
			Name:    name,
			StackId: env.Id,
			LaunchConfig: &client.LaunchConfig{
				Ports:     lbPorts,
				ImageUuid: imageUUID,
			},
			LbConfig: &client.LbConfig{},
		}

		lb, err = r.client.LoadBalancerService.Create(lb)
		if err != nil {
			return nil, fmt.Errorf("Unable to create load balancer for service %s. Error: %#v", name, err)
		}
	}

	err = r.setLBHosts(lb, hosts, service.Spec.Ports)
	if err != nil {
		return nil, err
	}

	if isValidToActivate(lb.State) {
		actionChannel := r.waitForLBAction("activate", lb)
		lbInterface, ok := <-actionChannel
		if !ok {
			return nil, fmt.Errorf("Couldn't call activate on LB %s", lb.Name)
		}
		lb = convertLB(lbInterface)
		_, err = r.client.LoadBalancerService.ActionActivate(lb)
		if err != nil {
			return nil, fmt.Errorf("Error creating LB %s. Couldn't activate LB. Error: %#v", name, err)
		}
	}

	lb, err = r.reloadLBService(lb)
	if err != nil {
		return nil, fmt.Errorf("Error creating LB %s. Couldn't reload LB to get status. Error: %#v", name, err)
	}

	// wait till service is active
	actionChannel := r.waitForLBAction("deactivate", lb)
	lbInterface, ok := <-actionChannel
	if !ok {
		return nil, fmt.Errorf("Timeout for service to become active %s", lb.Name)
	}
	lb = convertLB(lbInterface)

	epChannel := r.waitForLBPublicEndpoints(1, lb)
	_, ok = <-epChannel
	if !ok {
		return nil, fmt.Errorf("Couldn't get publicEndpoints for LB %s", name)
	}

	lb, err = r.reloadLBService(lb)
	if err != nil {
		return nil, fmt.Errorf("Error creating LB %s. Couldn't reload LB to get status. Error: %#v", name, err)
	}

	status, _, err := r.toLBStatus(lb)
	if err != nil {
		return nil, err
	}

	return status, nil
}

func (r *CloudProvider) GetSetting(key string) (string, bool) {
	opts := client.NewListOpts()
	opts.Filters["name"] = key
	settings, err := r.client.Setting.List(opts)
	if err != nil {
		glog.Errorf("GetSetting(%s): Error: %s", key, err)
		return "", false
	}

	for _, data := range settings.Data {
		if strings.EqualFold(data.Name, key) {
			return data.Value, true
		}
	}

	return "", false
}

func (r *CloudProvider) waitForLBPublicEndpoints(count int, lb *client.LoadBalancerService) <-chan interface{} {
	cb := func(result chan<- interface{}) (bool, error) {
		lb, err := r.reloadLBService(lb)
		if err != nil {
			return false, err
		}
		if len(lb.PublicEndpoints) >= count {
			result <- lb
			return true, nil
		}
		return false, nil
	}
	return r.waitForAction("publicEndpoints", cb)
}

func (r *CloudProvider) reloadLBService(lb *client.LoadBalancerService) (*client.LoadBalancerService, error) {
	lb, err := r.client.LoadBalancerService.ById(lb.Id)
	if err != nil {
		return nil, fmt.Errorf("Couldn't reload LB [%s]. Error: %#v", lb.Name, err)
	}
	return lb, nil
}

func convertLB(intf interface{}) *client.LoadBalancerService {
	lb, ok := intf.(*client.LoadBalancerService)
	if !ok {
		panic(fmt.Sprintf("Couldn't cast to LoadBalancerService type! Interface: %#v", intf))
	}
	return lb
}

// UpdateLoadBalancer is an implementation of LoadBalancer.UpdateLoadBalancer.
func (r *CloudProvider) UpdateLoadBalancer(clusterName string, service *api.Service, nodes []*api.Node) error {
	hosts := []string{}

	for _, node := range nodes {
		hosts = append(hosts, node.Name)
	}

	name := formatLBName(cloudprovider.GetLoadBalancerName(service))
	glog.Infof("UpdateLoadBalancer [%s] [%s]", name, hosts)
	lb, err := r.getLBByName(name)
	if err != nil {
		return err
	}

	if lb == nil {
		return fmt.Errorf("Couldn't find LB with name %s", name)
	}

	err = r.deleteLBConsumedServices(lb)
	if err != nil {
		return err
	}

	err = r.setLBHosts(lb, hosts, service.Spec.Ports)
	if err != nil {
		return err
	}

	return nil
}

// EnsureLoadBalancerDeleted is an implementation of LoadBalancer.EnsureLoadBalancerDeleted.
func (r *CloudProvider) EnsureLoadBalancerDeleted(clusterName string, service *api.Service) error {
	name := formatLBName(cloudprovider.GetLoadBalancerName(service))
	glog.Infof("EnsureLoadBalancerDeleted [%s]", name)
	lb, err := r.getLBByName(name)
	if err != nil {
		return err
	}

	if lb == nil {
		glog.Infof("Couldn't find LB %s to delete. Nothing to do.")
		return nil
	}

	return r.deleteLoadBalancer(lb)
}

func (r *CloudProvider) getOrCreateEnvironment() (*client.Stack, error) {
	opts := client.NewListOpts()
	opts.Filters["name"] = kubernetesEnvName
	opts.Filters["removed_null"] = "1"
	opts.Filters["external_id"] = kubernetesExternalId

	envs, err := r.client.Stack.List(opts)
	if err != nil {
		return nil, fmt.Errorf("Coudln't get host by name [%s]. Error: %#v", kubernetesEnvName, err)
	}

	if len(envs.Data) >= 1 {
		return &envs.Data[0], nil
	}

	env := &client.Stack{
		Name:       kubernetesEnvName,
		ExternalId: kubernetesExternalId,
	}

	env, err = r.client.Stack.Create(env)
	if err != nil {
		return nil, fmt.Errorf("Couldn't create stack for kubernetes LBs. Error: %#v", err)
	}
	return env, nil
}

func (r *CloudProvider) setLBHosts(lb *client.LoadBalancerService, hosts []string, ports []api.ServicePort) error {
	portRules := []client.PortRule{}
	for _, hostname := range hosts {
		extSvcName := buildExternalServiceName(hostname)
		opts := client.NewListOpts()
		opts.Filters["name"] = extSvcName
		opts.Filters["stackId"] = lb.StackId
		opts.Filters["removed_null"] = "1"

		exSvces, err := r.client.ExternalService.List(opts)
		if err != nil {
			return fmt.Errorf("Couldn't get external service %s for LB %s. Error: %#v.", extSvcName, lb.Name, err)
		}

		var exSvc *client.ExternalService
		if len(exSvces.Data) > 0 {
			exSvc = &exSvces.Data[0]
		} else {
			host, err := r.hostGetOrFetchFromCache(hostname)
			if err != nil {
				return fmt.Errorf("Couldn't create extrnal service [%s] for LB [%s]. Error: %#v", hostname, lb.Name, err)
			}

			if host.RancherHost.AgentIpAddress == "" {
				continue
			}

			exSvc = &client.ExternalService{
				Name:                extSvcName,
				ExternalIpAddresses: []string{host.RancherHost.AgentIpAddress},
				StackId:             lb.StackId,
			}
			exSvc, err = r.client.ExternalService.Create(exSvc)
			if err != nil {
				return fmt.Errorf("Error setting hosts for LB [%s]. Couldn't create external service for host [%s]. Error: %#v",
					lb.Name, extSvcName, err)
			}
		}

		if isValidToActivate(exSvc.State) {
			actionChannel := r.waitForSvcAction("activate", exSvc)
			svcInterface, ok := <-actionChannel
			if !ok {
				return fmt.Errorf("Couldn't call activate on external service [%s] for LB [%s] in a state [%s]", exSvc.Id, lb.Name, exSvc.State)
			}
			exSvc, ok = svcInterface.(*client.ExternalService)
			if !ok {
				panic(fmt.Sprintf("Couldn't cast to ExternalService type! Interface: %#v", svcInterface))
			}

			_, err = r.client.ExternalService.ActionActivate(exSvc)
			if err != nil {
				return fmt.Errorf("Couldn't activate service for LB [%s]. Error: %#v", lb.Name, err)
			}
		}
		for _, port := range ports {
			portRule := client.PortRule{
				SourcePort: int64(port.Port),
				TargetPort: int64(port.NodePort),
				ServiceId:  exSvc.Id,
				Protocol:   "tcp",
			}
			portRules = append(portRules, portRule)
		}
	}

	// service links are still used for dependency tracking
	// while all lb configuration is done via lbConfig/portRules
	actionChannel := r.waitForLBAction("setservicelinks", lb)
	lbInterface, ok := <-actionChannel
	if !ok {
		return fmt.Errorf("Couldn't call setservicelinks on LB %s", lb.Name)
	}
	lb = convertLB(lbInterface)

	toUpdate := make(map[string]interface{})
	updatedConfig := client.LbConfig{}
	updatedConfig.PortRules = portRules
	toUpdate["lbConfig"] = updatedConfig

	_, err := r.client.LoadBalancerService.Update(lb, toUpdate)
	if err != nil {
		return fmt.Errorf("Error updating port rules for LB [%s]. Error: %#v.", lb.Name, err)
	}

	return nil
}

func isValidToActivate(state string) bool {
	activeStates := []string{"active", "activating", "updating-active"}
	for _, activeState := range activeStates {
		if strings.EqualFold(state, activeState) {
			return false
		}
	}
	return true
}

func buildExternalServiceName(hostname string) string {
	cleaned := allowedChars.ReplaceAllString(hostname, "-")
	cleaned = strings.Trim(cleaned, "-")
	cleaned = dupeHyphen.ReplaceAllString(cleaned, "-")
	if len(cleaned) > 63 {
		cleaned = cleaned[:63]
	}
	return cleaned
}

type waitCallback func(result chan<- interface{}) (bool, error)

func (r *CloudProvider) waitForLBAction(action string, lb *client.LoadBalancerService) <-chan interface{} {
	cb := func(result chan<- interface{}) (bool, error) {
		l, err := r.client.LoadBalancerService.ById(lb.Id)
		if err != nil {
			return false, fmt.Errorf("Error waiting for action %s for LB %s. Couldn't get LB by id. Error: %#v.", action, lb.Name, err)
		}
		if _, ok := l.Actions[action]; ok {
			result <- l
			return true, nil
		}
		return false, nil
	}
	return r.waitForAction(action, cb)
}

func (r *CloudProvider) waitForSvcAction(action string, svc *client.ExternalService) <-chan interface{} {
	cb := func(result chan<- interface{}) (bool, error) {
		s, err := r.client.ExternalService.ById(svc.Id)
		if err != nil {
			return false, fmt.Errorf("Error waiting for action %s for service %s. Couldn't get service by id. Error %#v.", action, svc.Name, err)
		}
		if _, ok := s.Actions[action]; ok {
			result <- s
			return true, nil
		}
		return false, nil
	}
	return r.waitForAction(action, cb)
}

func (r *CloudProvider) waitForAction(action string, callback waitCallback) <-chan interface{} {
	ready := make(chan interface{}, 0)
	go func() {
		sleep := 2
		defer close(ready)
		for i := 0; i < 30; i++ {
			foundAction, err := callback(ready)
			if err != nil {
				glog.Errorf("Error: %#v", err)
				return
			}

			if foundAction {
				return
			}
			time.Sleep(time.Second * time.Duration(sleep))
		}
		glog.Errorf("Timed out waiting for action %s.", action)
	}()
	return ready
}

func (r *CloudProvider) getLBByName(name string) (*client.LoadBalancerService, error) {
	opts := client.NewListOpts()
	opts.Filters["name"] = name
	opts.Filters["removed_null"] = "1"
	lbs, err := r.client.LoadBalancerService.List(opts)
	if err != nil {
		return nil, fmt.Errorf("Coudln't get LB by name [%s]. Error: %#v", name, err)
	}

	if len(lbs.Data) == 0 {
		return nil, nil
	}

	if len(lbs.Data) > 1 {
		return nil, fmt.Errorf("Multiple LBs found for name: %s", name)
	}

	return &lbs.Data[0], nil
}

func convertObject(obj1 interface{}, obj2 interface{}) error {
	b, err := json.Marshal(obj1)
	if err != nil {
		return err
	}

	if err := json.Unmarshal(b, obj2); err != nil {
		return err
	}
	return nil
}

func (r *CloudProvider) toLBStatus(lb *client.LoadBalancerService) (*api.LoadBalancerStatus, bool, error) {
	eps := lb.PublicEndpoints

	ingress := []api.LoadBalancerIngress{}

	for _, epObj := range eps {
		ep := PublicEndpoint{}

		err := convertObject(epObj, &ep)
		if err != nil {
			return nil, false, err
		}
		ingress = append(ingress, api.LoadBalancerIngress{IP: ep.IPAddress})
	}

	return &api.LoadBalancerStatus{ingress}, true, nil
}

func (r *CloudProvider) deleteLoadBalancer(lb *client.LoadBalancerService) error {
	err := r.deleteLBConsumedServices(lb)
	if err != nil {
		return err
	}

	err = r.client.LoadBalancerService.Delete(lb)
	if err != nil {
		return fmt.Errorf("Unable to delete load balancer for service %s. Error: %#v", lb.Name, err)
	}
	return nil
}

func (r *CloudProvider) deleteLBConsumedServices(lb *client.LoadBalancerService) error {
	coll := &client.ServiceCollection{}
	err := r.client.GetLink(lb.Resource, "consumedservices", coll)
	if err != nil {
		return fmt.Errorf("Can't delete consumed services for LB %s. Error getting consumed services. Error: %#v", lb.Name, err)
	}

	for _, service := range coll.Data {
		consumedBy := &client.ServiceCollection{}
		err = r.client.GetLink(service.Resource, "consumedbyservices", consumedBy)
		if err != nil {
			glog.Errorf("Error getting consumedby services for service %s. This service won't be deleted. Error: %#v",
				service.Id, err)
			continue
		}

		if len(consumedBy.Data) > 1 {
			glog.Infof("Service %s has more than consumer. Will not delete it.", service.Id)
			continue
		}
		glog.Infof("Removing consumed external service [%s]", service.Name)
		err = r.client.Service.Delete(&service)
		if err != nil {
			glog.Warningf("Error deleting service %s. Moving on. Error: %#v", service.Id, err)
		}
	}

	return nil
}

// --- Instances Functions ---

// NodeAddresses returns the addresses of the specified instance.
// This implementation only returns the address of the calling instance. This is ok
// because the gce implementation makes that assumption and the comment for the interface
// states it as a todo to clarify that it is only for the current host
func (r *CloudProvider) NodeAddresses(nodeName types.NodeName) ([]api.NodeAddress, error) {
	host, err := r.hostGetOrFetchFromCache(string(nodeName))
	if err != nil {
		return nil, err
	}
	return []api.NodeAddress{
		{
			Type: api.NodeExternalIP,
			Address: host.RancherHost.AgentIpAddress,
		},
		{
			Type: api.NodeInternalIP,
			Address: host.RancherHost.AgentIpAddress,
		},
		{
			Type: api.NodeHostName,
			Address: host.RancherHost.Hostname,
		},
	}, nil
}

// ExternalID returns the cloud provider ID of the specified instance (deprecated).
func (r *CloudProvider) ExternalID(nodeName types.NodeName) (string, error) {
	name := string(nodeName)
	glog.Infof("ExternalID [%s]", name)
	return r.InstanceID(nodeName)
}

// InstanceID returns the cloud provider ID of the specified instance.
func (r *CloudProvider) InstanceID(nodeName types.NodeName) (string, error) {
	name := string(nodeName)
	glog.Infof("InstanceID [%s]", name)
	host, err := r.hostGetOrFetchFromCache(name)
	if err != nil {
		return "", err
	}

	return host.RancherHost.Uuid, nil
}

// InstanceType returns the type of the specified instance.
// Note that if the instance does not exist or is no longer running, we must return ("", cloudprovider.InstanceNotFound)
func (r *CloudProvider) InstanceType(nodeName types.NodeName) (string, error) {
	_, err := r.InstanceID(nodeName)
	if err != nil {
		return "", err
	}
	return providerName, nil
}

// InstanceTypeByProviderID returns the cloudprovider instance type of the node with the specified unique providerID
// This method will not be called from the node that is requesting this ID. i.e. metadata service
// and other local methods cannot be used here
func (r *CloudProvider) InstanceTypeByProviderID(providerID string) (string, error) {
	return "", errors.New("unimplemented")
}

// List lists instances that match 'filter' which is a regular expression which must match the entire instance name (fqdn)
func (r *CloudProvider) List(filter string) ([]types.NodeName, error) {
	glog.Infof("List %s", filter)

	opts := client.NewListOpts()
	opts.Filters["removed_null"] = "1"
	hosts, err := r.client.Host.List(opts)
	if err != nil {
		return nil, fmt.Errorf("Coudln't get hosts by filter [%s]. Error: %#v", filter, err)
	}

	if len(hosts.Data) == 0 {
		return nil, fmt.Errorf("No hosts found")
	}

	if strings.HasPrefix(filter, "'") && strings.HasSuffix(filter, "'") {
		filter = filter[1 : len(filter)-1]
	}

	re, err := regexp.Compile(filter)
	if err != nil {
		return nil, err
	}

	retHosts := []types.NodeName{}
	for _, host := range hosts.Data {
		if re.MatchString(host.Hostname) {
			retHosts = append(retHosts, types.NodeName(host.Hostname))
		}
	}

	return retHosts, err
}

// AddSSHKeyToAllInstances adds an SSH public key as a legal identity for all instances
// expected format for the key is standard ssh-keygen format: <protocol> <blob>
func (r *CloudProvider) AddSSHKeyToAllInstances(user string, keyData []byte) error {
	return fmt.Errorf("Not implemented")
}

// NodeAddressesByProviderID returns the node addresses of an instances with the specified unique providerID
// This method will not be called from the node that is requesting this ID. i.e. metadata service
// and other local methods cannot be used here
func (r *CloudProvider) NodeAddressesByProviderID(providerID string) ([]api.NodeAddress, error) {
	return []api.NodeAddress{}, errors.New("unimplemented")
}

// CurrentNodeName returns the name of the node we are currently running on
func (r *CloudProvider) CurrentNodeName(hostname string) (types.NodeName, error) {
	return types.NodeName(hostname), nil
}

func (r *CloudProvider) addHostToCache(host *Host) {
	if host != nil {
		r.hostCache.Add(host)
	}
}

func (r *CloudProvider) removeFromCache(name string) {
	host := r.getHostFromCache(name)
	if host != nil {
		r.hostCache.Delete(host)
	}
}

func (r *CloudProvider) getHostFromCache(name string) *Host {
	var host *Host

	// entry gets expired once retrieved
	defer r.addHostToCache(host)

	hostObj, exists, err := r.hostCache.GetByKey(name)
	if err == nil && exists {
		h, ok := hostObj.(*Host)
		if ok {
			host = h
		}
	}
	return host
}

func (r *CloudProvider) hostGetOrFetchFromCache(name string) (*Host, error) {
	host, err := r.getHostByName(name)
	if err != nil {
		if err == cloudprovider.InstanceNotFound {
			// evict from cache
			r.removeFromCache(name)
			return nil, err
		} else {
			host := r.getHostFromCache(name)
			if host != nil {
				return host, nil
			} else {
				return nil, err
			}
		}
	}
	r.addHostToCache(host)
	return host, nil
}

func (r *CloudProvider) getHostByName(name string) (*Host, error) {
	opts := client.NewListOpts()
	opts.Filters["removed_null"] = "1"
	hosts, err := r.client.Host.List(opts)
	if err != nil {
		return nil, fmt.Errorf("Coudln't get host by name [%s]. Error: %#v", name, err)
	}

	hostsToReturn := make([]client.Host, 0)
	fqdnParts := strings.Split(name, ".")
	hostname := name
	for _, host := range hosts.Data {
		rancherFQDNParts := strings.Split(host.Hostname, ".")
		rancherHostname := host.Hostname
		if len(rancherFQDNParts) > 1 {
			// rancher uses fqdn
			if len(fqdnParts) == 1 {
				// truncate rancher fqdn to hostname
				// if rancher uses fqdn but kubelet
				// uses hostname
				rancherHostname = rancherFQDNParts[0]
			}
		} else {
			// rancher uses hostname
			hostname = fqdnParts[0]
		}
		if strings.EqualFold(rancherHostname, hostname) {
			hostsToReturn = append(hostsToReturn, host)
		}
	}

	if len(hostsToReturn) == 0 {
		return nil, cloudprovider.InstanceNotFound
	}

	if len(hostsToReturn) > 1 {
		return nil, fmt.Errorf("multiple instances found for name: %s", name)
	}

	host := &Host{
		RancherHost: &hostsToReturn[0],
	}

	return host, nil
}

// --- Zones Functions ---

// GetZone is an implementation of Zones.GetZone
func (r *CloudProvider) GetZone() (cloudprovider.Zone, error) {
	return cloudprovider.Zone{
		FailureDomain: "FailureDomain1",
		Region:        "Region1",
	}, nil
}

// --- Utility functions ---

func init() {
	cloudprovider.RegisterCloudProvider(providerName, func(config io.Reader) (cloudprovider.Interface, error) {
		return newRancherCloud(config)
	})
}

type configGlobal struct {
	CattleURL       string `gcfg:"cattle-url"`
	CattleAccessKey string `gcfg:"cattle-access-key"`
	CattleSecretKey string `gcfg:"cattle-secret-key"`
}

type rConfig struct {
	Global configGlobal
}

func newRancherCloud(config io.Reader) (cloudprovider.Interface, error) {
	url := os.Getenv("CATTLE_URL")
	accessKey := os.Getenv("CATTLE_ACCESS_KEY")
	secretKey := os.Getenv("CATTLE_SECRET_KEY")
	conf := rConfig{
		Global: configGlobal{
			CattleURL:       url,
			CattleAccessKey: accessKey,
			CattleSecretKey: secretKey,
		},
	}
	client, err := getRancherClient(conf)
	if err != nil {
		return nil, fmt.Errorf("Could not create rancher client: %#v", err)
	}

	cache := cache.NewTTLStore(hostStoreKeyFunc, time.Duration(24)*time.Hour)

	return &CloudProvider{
		client:    client,
		conf:      &conf,
		hostCache: cache,
	}, nil
}

func hostStoreKeyFunc(obj interface{}) (string, error) {
	return obj.(*Host).RancherHost.Hostname, nil
}

func getRancherClient(conf rConfig) (*client.RancherClient, error) {
	return client.NewRancherClient(&client.ClientOpts{
		Url:       conf.Global.CattleURL,
		AccessKey: conf.Global.CattleAccessKey,
		SecretKey: conf.Global.CattleSecretKey,
	})
}

func (r *CloudProvider) get(url string) ([]byte, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("Couldn't get %s: Error creating request: %v", url, err)
	}
	req.Header.Add("Authorization", basicAuth(r.conf.Global.CattleAccessKey, r.conf.Global.CattleSecretKey))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Couldn't get %s: %v", url, err)
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("Error ready body of response to [%s]. Error %v", url, err)
	}

	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("Received unexpected response code for [%s]: [%v]. Response body: [%s]", url, resp.StatusCode, string(body))
	}

	return body, nil
}

func basicAuth(username, password string) string {
	auth := username + ":" + password
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(auth))
}

func metadata(path string) (string, error) {
	resp, err := http.Get("http://rancher-metadata/latest" + path)
	if err != nil {
		return "", fmt.Errorf("Couldn't get %s: %v", path, err)
	}

	body, err := ioutil.ReadAll(resp.Body)
	ret := string(body)
	if err != nil {
		return "", fmt.Errorf("Couldn't get %s: %v", path, err)
	}

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("Received unexpected response code: [%v], Response body: [%s]", resp.StatusCode, ret)
	}

	return ret, nil
}

func (r *CloudProvider) getJSON(url string, params map[string][]string, respObject interface{}) error {
	url, err := addParameters(url, params)
	if err != nil {
		return err
	}

	instanceRaw, err := r.get(url)
	if err != nil {
		return err
	}

	err = json.Unmarshal(instanceRaw, respObject)
	if err != nil {
		return fmt.Errorf("Couldn't unmarshal response json for [%s]. Error: %#v", url, err)
	}

	return nil
}

func addParameters(baseURL string, params map[string][]string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("Couldn't parse url [%s]. Error: %#v", baseURL, err)
	}
	q := u.Query()
	for key, vals := range params {
		for _, val := range vals {
			q.Add(key, val)
		}
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func portsChanged(newPorts []string, oldPorts []string) bool {
	if len(newPorts) != len(oldPorts) {
		return true
	}

	if len(newPorts) == 0 {
		return false
	}

	sort.Strings(newPorts)
	sort.Strings(oldPorts)
	for idx, p := range newPorts {
		if p != oldPorts[idx] {
			return true
		}
	}

	return false
}

func formatLBName(name string) string {
	return fmt.Sprintf(lbNameFormat, name)
}
