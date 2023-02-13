/*
SPDX-License-Identifier: Apache-2.0

Copyright Contributors to the Submariner project.

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

package serviceimport

import (
	"fmt"
	"strconv"
	"sync"

	"github.com/submariner-io/admiral/pkg/slices"
	"github.com/submariner-io/lighthouse/coredns/constants"
	"github.com/submariner-io/lighthouse/coredns/loadbalancer"
	mcsv1a1 "sigs.k8s.io/mcs-api/pkg/apis/v1alpha1"
)

type DNSRecord struct {
	IP          string
	Ports       []mcsv1a1.ServicePort
	HostName    string
	ClusterName string
}

type clusterInfo struct {
	record *DNSRecord
	name   string
	weight int64
}

type serviceInfo struct {
	records    map[string]*clusterInfo
	balancer   loadbalancer.Interface
	isHeadless bool
	ports      []mcsv1a1.ServicePort
}

func (si *serviceInfo) resetLoadBalancing() {
	si.balancer.RemoveAll()

	for _, info := range si.records {
		err := si.balancer.Add(info.name, info.weight)
		if err != nil {
			logger.Error(err, "Error adding load balancer info")
		}
	}
}

func (si *serviceInfo) newRecordFrom(from *DNSRecord) *DNSRecord {
	r := *from
	r.Ports = si.ports

	return &r
}

func (si *serviceInfo) mergePorts() {
	si.ports = nil

	for _, info := range si.records {
		if si.ports == nil {
			si.ports = info.record.Ports
		} else {
			si.ports = slices.Intersect(si.ports, info.record.Ports, func(p mcsv1a1.ServicePort) string {
				return fmt.Sprintf("%s%s%d", p.Name, p.Protocol, p.Port)
			})
		}
	}
}

type Map struct {
	svcMap         map[string]*serviceInfo
	localClusterID string
	mutex          sync.RWMutex
}

func (m *Map) selectIP(si *serviceInfo, name, namespace string, checkCluster func(string) bool,
	checkEndpoint func(string, string, string) bool,
) *DNSRecord {
	queueLength := si.balancer.ItemCount()
	for i := 0; i < queueLength; i++ {
		selectedName := si.balancer.Next().(string)
		info := si.records[selectedName]

		if checkCluster(info.name) && checkEndpoint(name, namespace, info.name) {
			return info.record
		}

		// Will Skip the selected name until a full "round" of the items is done
		si.balancer.Skip(selectedName)
	}

	return nil
}

func (m *Map) GetIP(namespace, name, cluster, localCluster string, checkCluster func(string) bool,
	checkEndpoint func(string, string, string) bool,
) (record *DNSRecord, found bool) {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	si, ok := m.svcMap[keyFunc(namespace, name)]
	if !ok || si.isHeadless {
		return nil, false
	}

	// If a clusterID is specified, we supply it even if the service is not there
	if cluster != "" {
		info, found := si.records[cluster]
		if !found {
			return nil, found
		}

		return info.record, found
	}

	// If we are aware of the local cluster
	// And we found some accessible IP, we shall return it
	if localCluster != "" {
		info, found := si.records[localCluster]
		if found && info != nil && checkEndpoint(name, namespace, localCluster) {
			return si.newRecordFrom(info.record), found
		}
	}

	// Fall back to selected load balancer (weighted/RR/etc) if service is not presented in the local cluster
	record = m.selectIP(si, name, namespace, checkCluster, checkEndpoint)

	if record != nil {
		return si.newRecordFrom(record), true
	}

	return nil, true
}

func NewMap(localClusterID string) *Map {
	return &Map{
		svcMap:         make(map[string]*serviceInfo),
		localClusterID: localClusterID,
	}
}

func (m *Map) Put(serviceImport *mcsv1a1.ServiceImport) {
	if name, ok := getSourceName(serviceImport); ok {
		namespace := getSourceNamespace(serviceImport)
		key := keyFunc(namespace, name)

		m.mutex.Lock()
		defer m.mutex.Unlock()

		remoteService, ok := m.svcMap[key]

		if !ok {
			remoteService = &serviceInfo{
				records:    make(map[string]*clusterInfo),
				balancer:   loadbalancer.NewSmoothWeightedRR(),
				isHeadless: serviceImport.Spec.Type == mcsv1a1.Headless,
			}
		}

		if serviceImport.Spec.Type == mcsv1a1.ClusterSetIP {
			clusterName := getSourceCluster(serviceImport)

			record := &DNSRecord{
				IP:          serviceImport.Spec.IPs[0],
				Ports:       serviceImport.Spec.Ports,
				ClusterName: clusterName,
			}

			remoteService.records[clusterName] = &clusterInfo{
				name:   clusterName,
				record: record,
				weight: getServiceWeightFrom(serviceImport, m.localClusterID),
			}
		}

		if !remoteService.isHeadless {
			remoteService.resetLoadBalancing()
		}

		remoteService.mergePorts()

		m.svcMap[key] = remoteService
	}
}

func (m *Map) Remove(serviceImport *mcsv1a1.ServiceImport) {
	if name, ok := getSourceName(serviceImport); ok {
		namespace := getSourceNamespace(serviceImport)
		key := keyFunc(namespace, name)

		m.mutex.Lock()
		defer m.mutex.Unlock()

		remoteService, ok := m.svcMap[key]
		if !ok {
			return
		}

		for _, info := range serviceImport.Status.Clusters {
			delete(remoteService.records, info.Cluster)
		}

		if len(remoteService.records) == 0 {
			delete(m.svcMap, key)
		} else if !remoteService.isHeadless {
			remoteService.resetLoadBalancing()
		}

		remoteService.mergePorts()
	}
}

func getServiceWeightFrom(si *mcsv1a1.ServiceImport, forClusterName string) int64 {
	weightKey := constants.LoadBalancerWeightAnnotationPrefix + "/" + forClusterName
	if val, ok := si.Annotations[weightKey]; ok {
		f, err := strconv.ParseInt(val, 0, 64)
		if err != nil {
			return f
		}

		logger.Errorf(err, "Error parsing the %q annotation from ServiceImport %q", weightKey, si.Name)
	}

	return 1 // Zero will cause no selection
}

func keyFunc(namespace, name string) string {
	return namespace + "/" + name
}

func getSourceName(from *mcsv1a1.ServiceImport) (string, bool) {
	name, ok := from.Labels[mcsv1a1.LabelServiceName]
	if ok {
		return name, true
	}

	name, ok = from.Annotations["origin-name"]
	return name, ok
}

func getSourceNamespace(from *mcsv1a1.ServiceImport) string {
	ns, ok := from.Labels[constants.LabelSourceNamespace]
	if ok {
		return ns
	}

	return from.Annotations["origin-namespace"]
}

func getSourceCluster(from *mcsv1a1.ServiceImport) string {
	c, ok := from.Labels[constants.MCSLabelSourceCluster]
	if ok {
		return c
	}

	return from.Labels["lighthouse.submariner.io/sourceCluster"]
}
