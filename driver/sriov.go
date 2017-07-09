package driver

import (
	"fmt"
	"strconv"

	"github.com/docker/go-plugins-helpers/network"

	log "github.com/Sirupsen/logrus"
)

const (
	SRIOV_ENABLED	= "enabled"
	SRIOV_DISABLED	= "disabled"
	sriovUnsupported = "unsupported"
)

type sriovNetwork struct {
	genNw			*genericNetwork
	vfDevList		[]string
	discoveredVFCount	int
	maxVFCount		int
	state			string
	vlan			int
}

var networks	map[string]*sriovNetwork

func checkVlanNwExist(vlan int) bool {
	for _, nw := range networks {
		if vlan != 0 && nw.vlan == vlan {
			return true
		}
	}
	return false
}

func (nw *sriovNetwork) CreateNetwork(d *driver, genNw *genericNetwork,
			       nid string, options map[string]string,
			       ipv4Data *network.IPAMData) error {
	var curVFs int
	var err error
	var vlan int

	ndevName := options[networkDevice]
	err = d.getNetworkByGateway(ipv4Data.Gateway)
	if err != nil {
		return err
	}

	if options[sriovVlan] != "" {
		vlan, _ = strconv.Atoi(options[sriovVlan])
		if vlan > 4095 {
			return fmt.Errorf("vlan id out of range")
		}
		if checkVlanNwExist(vlan) {
			return fmt.Errorf("vlan already exist")
		}
	}
	nw.genNw = genNw

	nw.maxVFCount, err = netdevGetMaxVFCount(ndevName)
	if err != nil {
		return err
	}
	curVFs, err = netdevGetEnabledVFCount(ndevName)
	if err != nil {
		return err
	}
	if curVFs != 0 {
		nw.state = SRIOV_ENABLED
	} else {
		nw.state = SRIOV_DISABLED
	}

	err = nw.DiscoverVFs()
	if err != nil {
		return err
	}
	// store vlan so that when VFs are attached to container, vlan will be set at that time
	nw.vlan = vlan
	if len(networks) == 0 {
		networks = make(map[string]*sriovNetwork)
	}
	networks[nid] = nw

	log.Debugf("SRIOV CreateNetwork : [%s] IPv4Data : [ %+v ]\n", nw.genNw.id, nw.genNw.IPv4Data)
	return nil
}

func (nw *sriovNetwork) disableSRIOV() {
	netdevDisableSRIOV(nw.genNw.ndevName)
	nw.state = SRIOV_DISABLED
	nw.vfDevList = nil
	nw.discoveredVFCount = 0
}

func (nw *sriovNetwork) DiscoverVFs() (error) {
	var err error

	if nw.state == SRIOV_DISABLED {
		err = netdevEnableSRIOV(nw.genNw.ndevName)
		if err != nil {
			return err
		}
		nw.state = SRIOV_ENABLED
	}

	// if we haven't discovered VFs yet, try to discover
	if nw.discoveredVFCount == 0 {
		nw.vfDevList, err = vfDevList(nw.genNw.ndevName)
		if err != nil {
			nw.disableSRIOV()
			return err
		}
		nw.discoveredVFCount = len(nw.vfDevList)
	}

	log.Debugf("DiscoverVF vfDev list length : [%d %d]",
		   len(nw.vfDevList), nw.discoveredVFCount)
	return nil
}

func (nw *sriovNetwork) AllocVF(parentNetdev string) (string, string) {
	var allocatedDev string
	var vfNetdevName string

	if len(nw.vfDevList) == 0 {
		return "", ""
	}

	// fetch the last element
	allocatedDev = nw.vfDevList[len(nw.vfDevList) - 1]

	vfNetdevName = vfNetdevNameFromParent(parentNetdev, allocatedDev)
	if vfNetdevName == "" {
		return "", ""
	}

	pciDevName := vfPCIDevNameFromVfDir(parentNetdev, allocatedDev)
	if pciDevName != "" {
		SetVFDefaultMacAddress(parentNetdev, allocatedDev, vfNetdevName)
		if nw.vlan > 0 {
			SetVFVlan(parentNetdev, allocatedDev, nw.vlan)
		}
		unbindVF(parentNetdev, pciDevName)
		bindVF(parentNetdev, pciDevName)
	}

	/* get the new name, as this name can change after unbind-bind sequence */
	vfNetdevName = vfNetdevNameFromParent(parentNetdev, allocatedDev)
	if vfNetdevName == "" {
		return "", ""
	}

	nw.vfDevList = nw.vfDevList[:len(nw.vfDevList) - 1]

	log.Debugf("AllocVF parent [ %+v ] vf:%v vfdev: %v, len %v",
		   parentNetdev, allocatedDev, vfNetdevName, len(nw.vfDevList))
	return allocatedDev, vfNetdevName
}

func (nw *sriovNetwork) FreeVF(name string) {
	log.Debugf("FreeVF %v", name)
	nw.vfDevList = append(nw.vfDevList, name)
}

func (nw *sriovNetwork) CreateEndpoint(r *network.CreateEndpointRequest) (*network.CreateEndpointResponse, error) {
	var netdevName string
	var vfName string

	vfName, netdevName = nw.AllocVF(nw.genNw.ndevName)
	if netdevName == "" {
		return nil, fmt.Errorf("All devices in use [ %s ].", r.NetworkID)
	}
	ndev := &ptEndpoint {
		devName: netdevName,
		vfName: vfName,
		Address: r.Interface.Address,
	}
	nw.genNw.ndevEndpoints[r.EndpointID] = ndev

	endpointInterface := &network.EndpointInterface{}
	if r.Interface.Address == "" {
		endpointInterface.Address = ndev.Address
	}
	if r.Interface.MacAddress == "" {
		//endpointInterface.MacAddress = ndev.HardwareAddr
	}
	resp := &network.CreateEndpointResponse{Interface: endpointInterface}

	log.Debugf("SRIOV CreateEndpoint resp interface: [ %+v ] ", resp.Interface)
	return resp, nil
}

func (nw *sriovNetwork) DeleteEndpoint(endpoint *ptEndpoint) {

	nw.FreeVF(endpoint.vfName)
	log.Debugf("DeleteEndpoint  vfDev list length -------------: [ %+d ]", len(nw.vfDevList))
}

func (nw *sriovNetwork) DeleteNetwork(d *driver, req *network.DeleteNetworkRequest) {
	nw.disableSRIOV()
	delete(networks, nw.genNw.id)
	log.Debugf("DeleteNetwork: total networks = %d", len(networks))
}
