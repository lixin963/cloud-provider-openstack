/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package openstack

import (
	"context"
	"net"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/layer3/routers"
	neutronports "github.com/gophercloud/gophercloud/openstack/networking/v2/ports"
	"github.com/gophercloud/gophercloud/pagination"

	"k8s.io/apimachinery/pkg/types"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/cloud-provider-openstack/pkg/metrics"
	"k8s.io/cloud-provider-openstack/pkg/util/errors"
	"k8s.io/klog/v2"
)

// Routes implements the cloudprovider.Routes for OpenStack clouds
type Routes struct {
	compute        *gophercloud.ServiceClient
	network        *gophercloud.ServiceClient
	opts           RouterOpts
	networkingOpts NetworkingOpts
}

var _ cloudprovider.Routes = &Routes{}

// NewRoutes creates a new instance of Routes
func NewRoutes(compute *gophercloud.ServiceClient, network *gophercloud.ServiceClient, opts RouterOpts, networkingOpts NetworkingOpts) (cloudprovider.Routes, error) {
	if opts.RouterID == "" {
		return nil, errors.ErrNoRouterID
	}

	return &Routes{
		compute:        compute,
		network:        network,
		opts:           opts,
		networkingOpts: networkingOpts,
	}, nil
}

// ListRoutes lists all managed routes that belong to the specified clusterName
func (r *Routes) ListRoutes(ctx context.Context, clusterName string) ([]*cloudprovider.Route, error) {
	klog.V(4).Infof("ListRoutes(%v)", clusterName)

	nodeNamesByAddr := make(map[string]types.NodeName)
	err := foreachServer(r.compute, servers.ListOpts{}, func(srv *servers.Server) (bool, error) {
		interfaces, err := getAttachedInterfacesByID(r.compute, srv.ID)
		if err != nil {
			return false, err
		}

		addrs, err := nodeAddresses(srv, interfaces, r.networkingOpts)
		if err != nil {
			return false, err
		}

		name := mapServerToNodeName(srv)
		for _, addr := range addrs {
			nodeNamesByAddr[addr.Address] = name
		}

		return true, nil
	})
	if err != nil {
		return nil, err
	}

	mc := metrics.NewMetricContext("router", "get")
	router, err := routers.Get(r.network, r.opts.RouterID).Extract()
	if mc.ObserveRequest(err) != nil {
		return nil, err
	}

	var routes []*cloudprovider.Route
	for _, item := range router.Routes {
		nodeName, foundNode := nodeNamesByAddr[item.NextHop]
		if !foundNode {
			nodeName = types.NodeName(item.NextHop)
		}
		route := cloudprovider.Route{
			Name:            item.DestinationCIDR,
			TargetNode:      nodeName, //contains the nexthop address if node was not found
			Blackhole:       !foundNode,
			DestinationCIDR: item.DestinationCIDR,
		}
		routes = append(routes, &route)
	}

	return routes, nil
}

func foreachServer(client *gophercloud.ServiceClient, opts servers.ListOptsBuilder, handler func(*servers.Server) (bool, error)) error {
	mc := metrics.NewMetricContext("server", "list")
	pager := servers.List(client, opts)

	err := pager.EachPage(func(page pagination.Page) (bool, error) {
		s, err := servers.ExtractServers(page)
		if err != nil {
			return false, err
		}
		for _, server := range s {
			ok, err := handler(&server)
			if !ok || err != nil {
				return false, err
			}
		}
		return true, nil
	})
	return mc.ObserveRequest(err)
}

func updateRoutes(network *gophercloud.ServiceClient, router *routers.Router, newRoutes []routers.Route) (func(), error) {
	origRoutes := router.Routes // shallow copy

	mc := metrics.NewMetricContext("router", "update")
	_, err := routers.Update(network, router.ID, routers.UpdateOpts{
		Routes: &newRoutes,
	}).Extract()
	if mc.ObserveRequest(err) != nil {
		return nil, err
	}

	unwinder := func() {
		klog.V(4).Infof("Reverting routes change to router %v", router.ID)
		mc := metrics.NewMetricContext("router", "update")
		_, err := routers.Update(network, router.ID, routers.UpdateOpts{
			Routes: &origRoutes,
		}).Extract()
		if mc.ObserveRequest(err) != nil {
			klog.Warningf("Unable to reset routes during error unwind: %v", err)
		}
	}

	return unwinder, nil
}

func updateAllowedAddressPairs(network *gophercloud.ServiceClient, port *neutronports.Port, newPairs []neutronports.AddressPair) (func(), error) {
	origPairs := port.AllowedAddressPairs // shallow copy

	mc := metrics.NewMetricContext("port", "update")
	_, err := neutronports.Update(network, port.ID, neutronports.UpdateOpts{
		AllowedAddressPairs: &newPairs,
	}).Extract()
	if mc.ObserveRequest(err) != nil {
		return nil, err
	}

	unwinder := func() {
		klog.V(4).Infof("Reverting allowed-address-pairs change to port %v", port.ID)
		mc := metrics.NewMetricContext("port", "update")
		_, err := neutronports.Update(network, port.ID, neutronports.UpdateOpts{
			AllowedAddressPairs: &origPairs,
		}).Extract()
		if mc.ObserveRequest(err) != nil {
			klog.Warningf("Unable to reset allowed-address-pairs during error unwind: %v", err)
		}
	}

	return unwinder, nil
}

// CreateRoute creates the described managed route
func (r *Routes) CreateRoute(ctx context.Context, clusterName string, nameHint string, route *cloudprovider.Route) error {
	klog.V(4).Infof("CreateRoute(%v, %v, %v)", clusterName, nameHint, route)

	onFailure := newCaller()

	ip, _, _ := net.ParseCIDR(route.DestinationCIDR)
	isCIDRv6 := ip.To4() == nil
	addr, err := getAddressByName(r.compute, route.TargetNode, isCIDRv6, r.networkingOpts)

	if err != nil {
		return err
	}

	klog.V(4).Infof("Using nexthop %v for node %v", addr, route.TargetNode)

	mc := metrics.NewMetricContext("router", "get")
	router, err := routers.Get(r.network, r.opts.RouterID).Extract()
	if mc.ObserveRequest(err) != nil {
		return err
	}

	routes := router.Routes

	for _, item := range routes {
		if item.DestinationCIDR == route.DestinationCIDR && item.NextHop == addr {
			klog.V(4).Infof("Skipping existing route: %v", route)
			return nil
		}
	}

	routes = append(routes, routers.Route{
		DestinationCIDR: route.DestinationCIDR,
		NextHop:         addr,
	})

	unwind, err := updateRoutes(r.network, router, routes)
	if err != nil {
		return err
	}
	defer onFailure.call(unwind)

	// get the port of addr on target node.
	portID, err := getPortIDByIP(r.compute, route.TargetNode, addr)
	if err != nil {
		return err
	}
	port, err := getPortByID(r.network, portID)
	if err != nil {
		return err
	}

	found := false
	for _, item := range port.AllowedAddressPairs {
		if item.IPAddress == route.DestinationCIDR {
			klog.V(4).Infof("Found existing allowed-address-pair: %v", item)
			found = true
			break
		}
	}

	if !found {
		newPairs := append(port.AllowedAddressPairs, neutronports.AddressPair{
			IPAddress: route.DestinationCIDR,
		})
		unwind, err := updateAllowedAddressPairs(r.network, port, newPairs)
		if err != nil {
			return err
		}
		defer onFailure.call(unwind)
	}

	klog.V(4).Infof("Route created: %v", route)
	onFailure.disarm()
	return nil
}

// DeleteRoute deletes the specified managed route
func (r *Routes) DeleteRoute(ctx context.Context, clusterName string, route *cloudprovider.Route) error {
	klog.V(4).Infof("DeleteRoute(%v, %v)", clusterName, route)

	onFailure := newCaller()

	ip, _, _ := net.ParseCIDR(route.DestinationCIDR)
	isCIDRv6 := ip.To4() == nil
	var addr string

	// Blackhole routes are orphaned and have no counterpart in OpenStack
	if !route.Blackhole {
		var err error
		addr, err = getAddressByName(r.compute, route.TargetNode, isCIDRv6, r.networkingOpts)
		if err != nil {
			return err
		}
	}

	mc := metrics.NewMetricContext("router", "get")
	router, err := routers.Get(r.network, r.opts.RouterID).Extract()
	if mc.ObserveRequest(err) != nil {
		return err
	}

	routes := router.Routes
	index := -1
	for i, item := range routes {
		if item.DestinationCIDR == route.DestinationCIDR && (item.NextHop == addr || route.Blackhole && item.NextHop == string(route.TargetNode)) {
			index = i
			break
		}
	}

	if index == -1 {
		klog.V(4).Infof("Skipping non-existent route: %v", route)
		return nil
	}

	// Delete element `index`
	routes[index] = routes[len(routes)-1]
	routes = routes[:len(routes)-1]

	unwind, err := updateRoutes(r.network, router, routes)
	// If this was a blackhole route we are done, there are no ports to update
	if err != nil || route.Blackhole {
		return err
	}
	defer onFailure.call(unwind)

	// get the port of addr on target node.
	portID, err := getPortIDByIP(r.compute, route.TargetNode, addr)
	if err != nil {
		return err
	}
	port, err := getPortByID(r.network, portID)
	if err != nil {
		return err
	}

	addrPairs := port.AllowedAddressPairs
	index = -1
	for i, item := range addrPairs {
		if item.IPAddress == route.DestinationCIDR {
			index = i
			break
		}
	}

	if index != -1 {
		// Delete element `index`
		addrPairs[index] = addrPairs[len(addrPairs)-1]
		addrPairs = addrPairs[:len(addrPairs)-1]

		unwind, err := updateAllowedAddressPairs(r.network, port, addrPairs)
		if err != nil {
			return err
		}
		defer onFailure.call(unwind)
	}

	klog.V(4).Infof("Route deleted: %v", route)
	onFailure.disarm()
	return nil
}

func getPortIDByIP(compute *gophercloud.ServiceClient, targetNode types.NodeName, ipAddress string) (string, error) {
	srv, err := getServerByName(compute, targetNode)
	if err != nil {
		return "", err
	}

	interfaces, err := getAttachedInterfacesByID(compute, srv.ID)
	if err != nil {
		return "", err
	}

	for _, intf := range interfaces {
		for _, fixedIP := range intf.FixedIPs {
			if fixedIP.IPAddress == ipAddress {
				return intf.PortID, nil
			}
		}
	}

	return "", errors.ErrNotFound
}

func getPortByID(client *gophercloud.ServiceClient, portID string) (*neutronports.Port, error) {
	mc := metrics.NewMetricContext("port", "get")
	targetPort, err := neutronports.Get(client, portID).Extract()
	if mc.ObserveRequest(err) != nil {
		return nil, err
	}

	if targetPort == nil {
		return nil, errors.ErrNotFound
	}

	return targetPort, nil
}