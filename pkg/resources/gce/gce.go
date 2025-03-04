/*
Copyright 2019 The Kubernetes Authors.

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

package gce

import (
	"context"
	"fmt"
	"strings"

	compute "google.golang.org/api/compute/v1"
	clouddns "google.golang.org/api/dns/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	"k8s.io/kops/pkg/dns"
	"k8s.io/kops/pkg/resources"
	"k8s.io/kops/upup/pkg/fi"
	"k8s.io/kops/upup/pkg/fi/cloudup/gce"
)

type gceListFn func() ([]*resources.Resource, error)

const (
	typeInstance             = "Instance"
	typeInstanceTemplate     = "InstanceTemplate"
	typeDisk                 = "Disk"
	typeInstanceGroupManager = "InstanceGroupManager"
	typeTargetPool           = "TargetPool"
	typeFirewallRule         = "FirewallRule"
	typeForwardingRule       = "ForwardingRule"
	typeAddress              = "Address"
	typeRoute                = "Route"
	typeSubnet               = "Subnet"
	typeRouter               = "Router"
	typeDNSRecord            = "DNSRecord"
)

// Maximum number of `-` separated tokens in a name
// Example: nodeport-external-to-node-ipv6
const maxPrefixTokens = 5

func ListResourcesGCE(gceCloud gce.GCECloud, clusterName string, region string) (map[string]*resources.Resource, error) {
	if region == "" {
		region = gceCloud.Region()
	}

	resources := make(map[string]*resources.Resource)

	d := &clusterDiscoveryGCE{
		cloud:       gceCloud,
		gceCloud:    gceCloud,
		clusterName: clusterName,
	}

	{
		// TODO: Only zones in api.Cluster object, if we have one?
		gceZones, err := d.gceCloud.Compute().Zones().List(context.Background(), d.gceCloud.Project())
		if err != nil {
			return nil, fmt.Errorf("error listing zones: %v", err)
		}
		for _, gceZone := range gceZones {
			u, err := gce.ParseGoogleCloudURL(gceZone.Region)
			if err != nil {
				return nil, err
			}
			if u.Name != region {
				continue
			}
			d.zones = append(d.zones, gceZone.Name)
		}
		if len(d.zones) == 0 {
			return nil, fmt.Errorf("unable to determine zones in region %q", region)
		}
		klog.Infof("Scanning zones: %v", d.zones)
	}

	listFunctions := []gceListFn{
		d.listGCEInstanceTemplates,
		d.listInstanceGroupManagersAndInstances,
		d.listTargetPools,
		d.listForwardingRules,
		d.listFirewallRules,
		d.listGCEDisks,
		d.listGCEDNSZone,
		// TODO: Find routes via instances (via instance groups)
		d.listAddresses,
		d.listSubnets,
		d.listRouters,
	}
	for _, fn := range listFunctions {
		resourceTrackers, err := fn()
		if err != nil {
			return nil, err
		}
		for _, t := range resourceTrackers {
			resources[t.Type+":"+t.ID] = t
		}
	}

	// We try to clean up orphaned routes.
	// Technically we still have a race condition here - until the master(s) are terminated, they will keep
	// creating routes.  Another option might be to have a post-destroy cleanup, and only remove routes with no target.
	{
		resourceTrackers, err := d.listRoutes(resources)
		if err != nil {
			return nil, err
		}
		for _, t := range resourceTrackers {
			resources[t.Type+":"+t.ID] = t
		}
	}

	for k, t := range resources {
		if t.Done {
			delete(resources, k)
		}
	}
	return resources, nil
}

type clusterDiscoveryGCE struct {
	cloud       fi.Cloud
	gceCloud    gce.GCECloud
	clusterName string

	instanceTemplates []*compute.InstanceTemplate
	zones             []string
}

func (d *clusterDiscoveryGCE) findInstanceTemplates() ([]*compute.InstanceTemplate, error) {
	if d.instanceTemplates != nil {
		return d.instanceTemplates, nil
	}

	instanceTemplates, err := gce.FindInstanceTemplates(d.gceCloud, d.clusterName)
	if err != nil {
		return nil, err
	}

	d.instanceTemplates = instanceTemplates
	return d.instanceTemplates, nil
}

func (d *clusterDiscoveryGCE) listGCEInstanceTemplates() ([]*resources.Resource, error) {
	var resourceTrackers []*resources.Resource

	templates, err := d.findInstanceTemplates()
	if err != nil {
		return nil, err
	}
	for _, t := range templates {
		selfLink := t.SelfLink // avoid closure-in-loop go-tcha
		resourceTracker := &resources.Resource{
			Name: t.Name,
			ID:   t.Name,
			Type: typeInstanceTemplate,
			Deleter: func(cloud fi.Cloud, r *resources.Resource) error {
				return gce.DeleteInstanceTemplate(d.gceCloud, selfLink)
			},
			Obj: t,
		}

		klog.V(4).Infof("Found resource: %s", t.SelfLink)
		resourceTrackers = append(resourceTrackers, resourceTracker)
	}

	return resourceTrackers, nil
}

func (d *clusterDiscoveryGCE) listInstanceGroupManagersAndInstances() ([]*resources.Resource, error) {
	c := d.gceCloud
	project := c.Project()

	var resourceTrackers []*resources.Resource

	instanceTemplates := make(map[string]*compute.InstanceTemplate)
	{
		templates, err := d.findInstanceTemplates()
		if err != nil {
			return nil, err
		}
		for _, t := range templates {
			instanceTemplates[t.SelfLink] = t
		}
	}

	ctx := context.Background()

	for _, zoneName := range d.zones {
		is, err := c.Compute().InstanceGroupManagers().List(ctx, project, zoneName)
		if err != nil {
			return nil, fmt.Errorf("error listing InstanceGroupManagers: %v", err)
		}
		for i := range is {
			mig := is[i] // avoid closure-in-loop go-tcha
			instanceTemplate := instanceTemplates[mig.InstanceTemplate]
			if instanceTemplate == nil {
				klog.V(2).Infof("Ignoring MIG with unmanaged InstanceTemplate: %s", mig.InstanceTemplate)
				continue
			}

			resourceTracker := &resources.Resource{
				Name:    mig.Name,
				ID:      zoneName + "/" + mig.Name,
				Type:    typeInstanceGroupManager,
				Deleter: func(cloud fi.Cloud, r *resources.Resource) error { return gce.DeleteInstanceGroupManager(c, mig) },
				Obj:     mig,
			}

			resourceTracker.Blocks = append(resourceTracker.Blocks, typeInstanceTemplate+":"+instanceTemplate.Name)

			klog.V(4).Infof("Found resource: %s", mig.SelfLink)
			resourceTrackers = append(resourceTrackers, resourceTracker)

			instanceTrackers, err := d.listManagedInstances(mig)
			if err != nil {
				return nil, fmt.Errorf("error listing instances in InstanceGroupManager: %v", err)
			}
			resourceTrackers = append(resourceTrackers, instanceTrackers...)
		}
	}

	return resourceTrackers, nil
}

func (d *clusterDiscoveryGCE) listManagedInstances(igm *compute.InstanceGroupManager) ([]*resources.Resource, error) {
	c := d.gceCloud

	var resourceTrackers []*resources.Resource

	zoneName := gce.LastComponent(igm.Zone)

	instances, err := gce.ListManagedInstances(c, igm)
	if err != nil {
		return nil, err
	}

	for _, i := range instances {
		url := i.Instance // avoid closure-in-loop go-tcha
		name := gce.LastComponent(url)

		resourceTracker := &resources.Resource{
			Name: name,
			ID:   zoneName + "/" + name,
			Type: typeInstance,
			Deleter: func(cloud fi.Cloud, tracker *resources.Resource) error {
				return gce.DeleteInstance(c, url)
			},
			Dumper: DumpManagedInstance,
			Obj:    i,
		}

		// We don't block deletion of the instance group manager

		resourceTrackers = append(resourceTrackers, resourceTracker)
	}

	return resourceTrackers, nil
}

// findGCEDisks finds all Disks that are associated with the current cluster
// It matches them by looking for the cluster label
func (d *clusterDiscoveryGCE) findGCEDisks() ([]*compute.Disk, error) {
	c := d.gceCloud

	clusterTag := gce.SafeClusterName(d.clusterName)

	var matches []*compute.Disk

	ctx := context.Background()

	// TODO: Push down tag filter?

	diskLists, err := c.Compute().Disks().AggregatedList(ctx, c.Project())
	if err != nil {
		return nil, fmt.Errorf("error listing disks: %v", err)
	}

	for _, list := range diskLists {
		for _, d := range list.Disks {
			match := false
			for k, v := range d.Labels {
				if k == gce.GceLabelNameKubernetesCluster {
					if v == clusterTag {
						match = true
					} else {
						match = false
						break
					}
				}
			}

			if !match {
				continue
			}

			matches = append(matches, d)
		}
	}

	return matches, nil
}

func (d *clusterDiscoveryGCE) listGCEDisks() ([]*resources.Resource, error) {
	var resourceTrackers []*resources.Resource

	disks, err := d.findGCEDisks()
	if err != nil {
		return nil, err
	}
	for _, t := range disks {
		resourceTracker := &resources.Resource{
			Name:    t.Name,
			ID:      t.Name,
			Type:    typeDisk,
			Deleter: deleteGCEDisk,
			Obj:     t,
		}

		for _, u := range t.Users {
			resourceTracker.Blocked = append(resourceTracker.Blocked, typeInstance+":"+gce.LastComponent(t.Zone)+"/"+gce.LastComponent(u))
		}

		klog.V(4).Infof("Found resource: %s", t.SelfLink)
		resourceTrackers = append(resourceTrackers, resourceTracker)
	}

	return resourceTrackers, nil
}

func deleteGCEDisk(cloud fi.Cloud, r *resources.Resource) error {
	c := cloud.(gce.GCECloud)
	t := r.Obj.(*compute.Disk)

	klog.V(2).Infof("Deleting GCE Disk %s", t.SelfLink)
	u, err := gce.ParseGoogleCloudURL(t.SelfLink)
	if err != nil {
		return err
	}

	op, err := c.Compute().Disks().Delete(u.Project, u.Zone, u.Name)
	if err != nil {
		if gce.IsNotFound(err) {
			klog.Infof("disk not found, assuming deleted: %q", t.SelfLink)
			return nil
		}
		return fmt.Errorf("error deleting disk %s: %v", t.SelfLink, err)
	}

	return c.WaitForOp(op)
}

func (d *clusterDiscoveryGCE) listTargetPools() ([]*resources.Resource, error) {
	c := d.gceCloud

	var resourceTrackers []*resources.Resource

	ctx := context.Background()

	tps, err := c.Compute().TargetPools().List(ctx, c.Project(), c.Region())
	if err != nil {
		return nil, fmt.Errorf("error listing TargetPools: %v", err)
	}

	for _, tp := range tps {
		if !d.matchesClusterName(tp.Name) {
			continue
		}

		resourceTracker := &resources.Resource{
			Name:    tp.Name,
			ID:      tp.Name,
			Type:    typeTargetPool,
			Deleter: deleteTargetPool,
			Obj:     tp,
		}

		klog.V(4).Infof("Found resource: %s", tp.SelfLink)
		resourceTrackers = append(resourceTrackers, resourceTracker)
	}

	return resourceTrackers, nil
}

func deleteTargetPool(cloud fi.Cloud, r *resources.Resource) error {
	c := cloud.(gce.GCECloud)
	t := r.Obj.(*compute.TargetPool)

	klog.V(2).Infof("Deleting GCE TargetPool %s", t.SelfLink)
	u, err := gce.ParseGoogleCloudURL(t.SelfLink)
	if err != nil {
		return err
	}

	op, err := c.Compute().TargetPools().Delete(u.Project, u.Region, u.Name)
	if err != nil {
		if gce.IsNotFound(err) {
			klog.Infof("TargetPool not found, assuming deleted: %q", t.SelfLink)
			return nil
		}
		return fmt.Errorf("error deleting TargetPool %s: %v", t.SelfLink, err)
	}

	return c.WaitForOp(op)
}

func (d *clusterDiscoveryGCE) listForwardingRules() ([]*resources.Resource, error) {
	c := d.gceCloud

	var resourceTrackers []*resources.Resource

	ctx := context.Background()

	frs, err := c.Compute().ForwardingRules().List(ctx, c.Project(), c.Region())
	if err != nil {
		return nil, fmt.Errorf("error listing ForwardingRules: %v", err)
	}

	for _, fr := range frs {
		if !d.matchesClusterName(fr.Name) {
			continue
		}

		resourceTracker := &resources.Resource{
			Name:    fr.Name,
			ID:      fr.Name,
			Type:    typeForwardingRule,
			Deleter: deleteForwardingRule,
			Obj:     fr,
		}

		if fr.Target != "" {
			resourceTracker.Blocks = append(resourceTracker.Blocks, typeTargetPool+":"+gce.LastComponent(fr.Target))
		}

		if fr.IPAddress != "" {
			resourceTracker.Blocks = append(resourceTracker.Blocks, typeAddress+":"+gce.LastComponent(fr.IPAddress))
		}

		klog.V(4).Infof("Found resource: %s", fr.SelfLink)
		resourceTrackers = append(resourceTrackers, resourceTracker)
	}

	return resourceTrackers, nil
}

func deleteForwardingRule(cloud fi.Cloud, r *resources.Resource) error {
	c := cloud.(gce.GCECloud)
	t := r.Obj.(*compute.ForwardingRule)

	klog.V(2).Infof("Deleting GCE ForwardingRule %s", t.SelfLink)
	u, err := gce.ParseGoogleCloudURL(t.SelfLink)
	if err != nil {
		return err
	}

	op, err := c.Compute().ForwardingRules().Delete(u.Project, u.Region, u.Name)
	if err != nil {
		if gce.IsNotFound(err) {
			klog.Infof("ForwardingRule not found, assuming deleted: %q", t.SelfLink)
			return nil
		}
		return fmt.Errorf("error deleting ForwardingRule %s: %v", t.SelfLink, err)
	}

	return c.WaitForOp(op)
}

// listFirewallRules discovers Firewall objects for the cluster
func (d *clusterDiscoveryGCE) listFirewallRules() ([]*resources.Resource, error) {
	c := d.gceCloud

	var resourceTrackers []*resources.Resource

	ctx := context.Background()

	frs, err := c.Compute().Firewalls().List(ctx, c.Project())
	if err != nil {
		return nil, fmt.Errorf("error listing FirewallRules: %v", err)
	}

	for _, fr := range frs {
		if !d.matchesClusterNameMultipart(fr.Name, maxPrefixTokens) {
			continue
		}

		foundMatchingTarget := false
		tagPrefix := gce.SafeClusterName(d.clusterName) + "-"
		for _, target := range fr.TargetTags {
			if strings.HasPrefix(target, tagPrefix) {
				foundMatchingTarget = true
			}
		}
		if !foundMatchingTarget {
			break
		}

		resourceTracker := &resources.Resource{
			Name:    fr.Name,
			ID:      fr.Name,
			Type:    typeFirewallRule,
			Deleter: deleteFirewallRule,
			Obj:     fr,
		}

		klog.V(4).Infof("Found resource: %s", fr.SelfLink)
		resourceTrackers = append(resourceTrackers, resourceTracker)
	}

	return resourceTrackers, nil
}

// deleteFirewallRule is the helper function to delete a Resource for a Firewall object
func deleteFirewallRule(cloud fi.Cloud, r *resources.Resource) error {
	c := cloud.(gce.GCECloud)
	t := r.Obj.(*compute.Firewall)

	klog.V(2).Infof("Deleting GCE FirewallRule %s", t.SelfLink)
	u, err := gce.ParseGoogleCloudURL(t.SelfLink)
	if err != nil {
		return err
	}

	op, err := c.Compute().Firewalls().Delete(u.Project, u.Name)
	if err != nil {
		if gce.IsNotFound(err) {
			klog.Infof("FirewallRule not found, assuming deleted: %q", t.SelfLink)
			return nil
		}
		return fmt.Errorf("error deleting FirewallRule %s: %v", t.SelfLink, err)
	}

	return c.WaitForOp(op)
}

func (d *clusterDiscoveryGCE) listRoutes(resourceMap map[string]*resources.Resource) ([]*resources.Resource, error) {
	c := d.gceCloud

	var resourceTrackers []*resources.Resource

	instances := sets.NewString()
	for _, resource := range resourceMap {
		if resource.Type == typeInstance {
			instances.Insert(resource.ID)
		}
	}

	prefix := gce.SafeClusterName(d.clusterName) + "-"

	ctx := context.Background()

	// TODO: Push-down prefix?
	routes, err := c.Compute().Routes().List(ctx, c.Project())
	if err != nil {
		return nil, fmt.Errorf("error listing Routes: %v", err)
	}
	for _, r := range routes {
		if !strings.HasPrefix(r.Name, prefix) {
			continue
		}
		remove := false
		for _, w := range r.Warnings {
			switch w.Code {
			case "NEXT_HOP_INSTANCE_NOT_FOUND":
				remove = true
			default:
				klog.Infof("Unknown warning on route %q: %q", r.Name, w.Code)
			}
		}

		if r.NextHopInstance != "" {
			u, err := gce.ParseGoogleCloudURL(r.NextHopInstance)
			if err != nil {
				klog.Warningf("error parsing URL for NextHopInstance=%q", r.NextHopInstance)
			}

			if instances.Has(u.Zone + "/" + u.Name) {
				remove = true
			}
		}

		if remove {
			resourceTracker := &resources.Resource{
				Name:    r.Name,
				ID:      r.Name,
				Type:    typeRoute,
				Deleter: deleteRoute,
				Obj:     r,
			}

			// We don't need to block
			//if r.NextHopInstance != "" {
			//	resourceTracker.Blocked = append(resourceTracker.Blocks, typeInstance+":"+gce.LastComponent(r.NextHopInstance))
			//}

			klog.V(4).Infof("Found resource: %s", r.SelfLink)
			resourceTrackers = append(resourceTrackers, resourceTracker)
		}
	}

	return resourceTrackers, nil
}

func deleteRoute(cloud fi.Cloud, r *resources.Resource) error {
	c := cloud.(gce.GCECloud)
	t := r.Obj.(*compute.Route)

	klog.V(2).Infof("Deleting GCE Route %s", t.SelfLink)
	u, err := gce.ParseGoogleCloudURL(t.SelfLink)
	if err != nil {
		return err
	}

	op, err := c.Compute().Routes().Delete(u.Project, u.Name)
	if err != nil {
		if gce.IsNotFound(err) {
			klog.Infof("Route not found, assuming deleted: %q", t.SelfLink)
			return nil
		}
		return fmt.Errorf("error deleting Route %s: %v", t.SelfLink, err)
	}

	return c.WaitForOp(op)
}

func (d *clusterDiscoveryGCE) listAddresses() ([]*resources.Resource, error) {
	c := d.gceCloud

	var resourceTrackers []*resources.Resource

	ctx := context.Background()

	addrs, err := c.Compute().Addresses().List(ctx, c.Project(), c.Region())
	if err != nil {
		return nil, fmt.Errorf("error listing Addresses: %v", err)
	}

	for _, a := range addrs {
		if !d.matchesClusterName(a.Name) {
			klog.V(8).Infof("Skipping Address with name %q", a.Name)
			continue
		}

		resourceTracker := &resources.Resource{
			Name:    a.Name,
			ID:      a.Name,
			Type:    typeAddress,
			Deleter: deleteAddress,
			Obj:     a,
		}

		klog.V(4).Infof("Found resource: %s", a.SelfLink)
		resourceTrackers = append(resourceTrackers, resourceTracker)
	}

	return resourceTrackers, nil
}

func deleteAddress(cloud fi.Cloud, r *resources.Resource) error {
	c := cloud.(gce.GCECloud)
	t := r.Obj.(*compute.Address)

	klog.V(2).Infof("Deleting GCE Address %s", t.SelfLink)
	u, err := gce.ParseGoogleCloudURL(t.SelfLink)
	if err != nil {
		return err
	}

	op, err := c.Compute().Addresses().Delete(u.Project, u.Region, u.Name)
	if err != nil {
		if gce.IsNotFound(err) {
			klog.Infof("Address not found, assuming deleted: %q", t.SelfLink)
			return nil
		}
		return fmt.Errorf("error deleting Address %s: %v", t.SelfLink, err)
	}

	return c.WaitForOp(op)
}

func (d *clusterDiscoveryGCE) listSubnets() ([]*resources.Resource, error) {
	// Templates are very accurate because of the metadata, so use those as the sanity check
	templates, err := d.findInstanceTemplates()
	if err != nil {
		return nil, err
	}
	subnetworkUrls := make(map[string]bool)
	for _, t := range templates {
		for _, ni := range t.Properties.NetworkInterfaces {
			if ni.Subnetwork != "" {
				subnetworkUrls[ni.Subnetwork] = true
			}
		}
	}

	c := d.gceCloud

	var resourceTrackers []*resources.Resource
	ctx := context.Background()

	subnets, err := c.Compute().Subnetworks().List(ctx, c.Project(), c.Region())
	if err != nil {
		return nil, fmt.Errorf("error listing subnetworks: %v", err)
	}

	for _, o := range subnets {
		if !d.matchesClusterName(o.Name) {
			klog.V(8).Infof("skipping Subnet with name %q", o.Name)
			continue
		}

		if !subnetworkUrls[o.SelfLink] {
			klog.Warningf("skipping subnetwork %q because it didn't match any instance template", o.SelfLink)
			continue
		}

		resourceTracker := &resources.Resource{
			Name:    o.Name,
			ID:      o.Name,
			Type:    typeSubnet,
			Deleter: deleteSubnet,
			Obj:     o,
		}

		klog.V(4).Infof("found resource: %s", o.SelfLink)
		resourceTrackers = append(resourceTrackers, resourceTracker)
	}

	return resourceTrackers, nil
}

func deleteSubnet(cloud fi.Cloud, r *resources.Resource) error {
	c := cloud.(gce.GCECloud)
	o := r.Obj.(*compute.Subnetwork)

	klog.V(2).Infof("deleting GCE subnetwork %s", o.SelfLink)
	u, err := gce.ParseGoogleCloudURL(o.SelfLink)
	if err != nil {
		return err
	}

	op, err := c.Compute().Subnetworks().Delete(u.Project, u.Region, u.Name)
	if err != nil {
		if gce.IsNotFound(err) {
			klog.Infof("subnetwork not found, assuming deleted: %q", o.SelfLink)
			return nil
		}
		return fmt.Errorf("error deleting subnetwork %s: %v", o.SelfLink, err)
	}

	return c.WaitForOp(op)
}

func (d *clusterDiscoveryGCE) listRouters() ([]*resources.Resource, error) {
	c := d.gceCloud

	var resourceTrackers []*resources.Resource
	ctx := context.Background()

	routers, err := c.Compute().Routers().List(ctx, c.Project(), c.Region())
	if err != nil {
		return nil, fmt.Errorf("error listing routers: %v", err)
	}

	for _, o := range routers {
		if !d.matchesClusterName(o.Name) {
			klog.V(8).Infof("skipping Router with name %q", o.Name)
			continue
		}

		resourceTracker := &resources.Resource{
			Name:    o.Name,
			ID:      o.Name,
			Type:    typeRouter,
			Deleter: deleteRouter,
			Obj:     o,
		}

		klog.V(4).Infof("found resource: %s", o.SelfLink)
		resourceTrackers = append(resourceTrackers, resourceTracker)
	}

	return resourceTrackers, nil
}

func deleteRouter(cloud fi.Cloud, r *resources.Resource) error {
	c := cloud.(gce.GCECloud)
	o := r.Obj.(*compute.Router)

	klog.V(2).Infof("deleting GCE router %s", o.SelfLink)
	u, err := gce.ParseGoogleCloudURL(o.SelfLink)
	if err != nil {
		return err
	}

	op, err := c.Compute().Routers().Delete(u.Project, u.Region, u.Name)
	if err != nil {
		if gce.IsNotFound(err) {
			klog.Infof("router not found, assuming deleted: %q", o.SelfLink)
			return nil
		}
		return fmt.Errorf("error deleting router %s: %v", o.SelfLink, err)
	}

	return c.WaitForOp(op)
}

func (d *clusterDiscoveryGCE) matchesClusterName(name string) bool {
	return d.matchesClusterNameMultipart(name, 1)
}

// matchesClusterNameMultipart checks if the name could have been generated by our cluster
// considering all the prefixes separated by `-`.  maxParts limits the number of parts we consider.
func (d *clusterDiscoveryGCE) matchesClusterNameMultipart(name string, maxParts int) bool {
	tokens := strings.Split(name, "-")

	for i := 1; i <= maxParts; i++ {
		if i > len(tokens) {
			break
		}

		id := strings.Join(tokens[:i], "-")
		if id == "" {
			continue
		}
		if name == gce.SafeObjectName(id, d.clusterName) {
			return true
		}
	}
	return false
}

func (d *clusterDiscoveryGCE) clusterDNSName() string {
	return d.clusterName + "."
}

func (d *clusterDiscoveryGCE) isKopsManagedDNSName(name string) bool {
	prefix := []string{`api`, `api.internal`, `bastion`}
	for _, p := range prefix {
		if name == p+"."+d.clusterDNSName() {
			return true
		}
	}
	return false
}

func (d *clusterDiscoveryGCE) listGCEDNSZone() ([]*resources.Resource, error) {

	if dns.IsGossipHostname(d.clusterName) {
		return nil, nil
	}

	var resourceTrackers []*resources.Resource

	managedZones, err := d.gceCloud.CloudDNS().ManagedZones().List(d.gceCloud.Project())
	if err != nil {
		return nil, fmt.Errorf("error getting GCE DNS zones %v", err)
	}

	for _, zone := range managedZones {
		if !strings.HasSuffix(d.clusterDNSName(), zone.DnsName) {
			continue
		}
		rrsets, err := d.gceCloud.CloudDNS().ResourceRecordSets().List(d.gceCloud.Project(), zone.Name)
		if err != nil {
			return nil, fmt.Errorf("error getting GCE DNS zone data %v", err)
		}

		for _, record := range rrsets {
			// adapted from AWS implementation
			if record.Type != "A" {
				continue
			}

			if d.isKopsManagedDNSName(record.Name) {
				resource := resources.Resource{
					Name:         record.Name,
					ID:           record.Name,
					Type:         typeDNSRecord,
					GroupDeleter: deleteDNSRecords,
					GroupKey:     zone.Name,
					Obj:          record,
				}
				resourceTrackers = append(resourceTrackers, &resource)
			}
		}
	}

	return resourceTrackers, nil
}

func deleteDNSRecords(cloud fi.Cloud, r []*resources.Resource) error {
	c := cloud.(gce.GCECloud)
	var records []*clouddns.ResourceRecordSet
	var zoneName string

	for _, record := range r {
		r := record.Obj.(*clouddns.ResourceRecordSet)
		zoneName = record.GroupKey
		records = append(records, r)
	}

	change := clouddns.Change{Deletions: records, Kind: "dns#change", IsServing: true}
	_, err := c.CloudDNS().Changes().Create(c.Project(), zoneName, &change)
	if err != nil {
		return fmt.Errorf("error deleting GCE DNS resource record set %v", err)
	}
	return nil
}
