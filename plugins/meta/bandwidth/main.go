// Copyright 2018 CNI authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package bandwidth

import (
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/types/current"
	"github.com/containernetworking/cni/pkg/version"

	"github.com/vishvananda/netlink"
)

type PluginConf struct {
	types.NetConf
	RuntimeConfig *struct{} `json:"runtimeConfig"`

	RawPrevResult *map[string]interface{} `json:"prevResult"`
	PrevResult    *current.Result         `json:"-"`

	IngressRate  int `json:"ingressRate"`  //Bandwidth rate in Kbps for traffic through container. 0 for no limit. If ingressRate is set, ingressBurst must also be set
	IngressBurst int `json:"ingressBurst"` //Bandwidth burst in Kb for traffic through container. 0 for no limit. If ingressBurst is set, ingressRate must also be set

	EgressRate  int `json:"egressRate"`  //Bandwidth rate in Kbps for traffic through container. 0 for no limit. If egressRate is set, egressBurst must also be set
	EgressBurst int `json:"egressBurst"` //Bandwidth burst in Kb for traffic through container. 0 for no limit. If egressBurst is set, egressRate must also be set
}

// parseConfig parses the supplied configuration (and prevResult) from stdin.
func parseConfig(stdin []byte) (*PluginConf, error) {
	conf := PluginConf{}

	if err := json.Unmarshal(stdin, &conf); err != nil {
		return nil, fmt.Errorf("failed to parse network configuration: %v", err)
	}

	if conf.RawPrevResult != nil {
		resultBytes, err := json.Marshal(conf.RawPrevResult)
		if err != nil {
			return nil, fmt.Errorf("could not serialize prevResult: %v", err)
		}
		res, err := version.NewResult(conf.CNIVersion, resultBytes)
		if err != nil {
			return nil, fmt.Errorf("could not parse prevResult: %v", err)
		}
		conf.RawPrevResult = nil
		conf.PrevResult, err = current.NewResultFromResult(res)
		if err != nil {
			return nil, fmt.Errorf("could not convert result to current version: %v", err)
		}
	}

	err := validateRateAndBurst(conf.IngressRate, conf.IngressBurst)
	if err != nil {
		return nil, err
	}
	err = validateRateAndBurst(conf.EgressRate, conf.EgressBurst)
	if err != nil {
		return nil, err
	}

	return &conf, nil

}

func validateRateAndBurst(rate int, burst int) error {
	switch {
	case burst < 0 || rate < 0:
		return fmt.Errorf("rate and burst must be a positive integer")
	case burst == 0 && rate != 0:
		return fmt.Errorf("if rate is set, burst must also be set")
	case rate == 0 && burst != 0:
		return fmt.Errorf("if burst is set, rate must also be set")
	}

	return nil
}

func getIfbDeviceName(networkName string, containerId string) (string, error) {
	hash := sha1.New()
	_, err := hash.Write([]byte(networkName + containerId))
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("%x", hash.Sum(nil))[:4], nil
}

func getMTU(deviceName string) (int, error) {
	link, err := netlink.LinkByName(deviceName)
	if err != nil {
		return -1, err
	}

	return link.Attrs().MTU, nil
}

func getHostInterface(interfaces []*current.Interface) (*current.Interface, error) {
	for _, prevIface := range interfaces {
		if prevIface.Sandbox != "" {
			continue
		}

		return prevIface, nil
	}

	return nil, errors.New("no host interface found")
}

func cmdAdd(args *skel.CmdArgs) error {
	conf, err := parseConfig(args.StdinData)
	if err != nil {
		return err
	}

	//no traffic shaping was requested, so just no-op and quit
	if conf.IngressRate == 0 && conf.IngressBurst == 0 && conf.EgressRate == 0 && conf.EgressBurst == 0 {
		return types.PrintResult(conf.PrevResult, conf.CNIVersion)
	}

	if conf.PrevResult == nil {
		return fmt.Errorf("must be called as chained plugin")
	}

	hostInterface, err := getHostInterface(conf.PrevResult.Interfaces)
	if err != nil {
		return err
	}

	if conf.IngressRate > 0 && conf.IngressBurst > 0 {
		err = CreateIngressQdisc(conf.IngressRate, conf.IngressBurst, hostInterface.Name)
		if err != nil {
			return err
		}
	}

	if conf.EgressRate > 0 && conf.EgressBurst > 0 {
		mtu, err := getMTU(hostInterface.Name)
		if err != nil {
			return err
		}

		ifbDeviceName, err := getIfbDeviceName(conf.Name, args.ContainerID)
		if err != nil {
			return err
		}

		err = CreateIfb(ifbDeviceName, mtu)
		if err != nil {
			return err
		}

		ifbDevice, err := netlink.LinkByName(ifbDeviceName)
		if err != nil {
			return err
		}

		conf.PrevResult.Interfaces = append(conf.PrevResult.Interfaces, &current.Interface{
			Name: ifbDeviceName,
			Mac:  ifbDevice.Attrs().HardwareAddr.String(),
		})
		err = CreateEgressQdisc(conf.EgressRate, conf.EgressBurst, hostInterface.Name, ifbDeviceName)
		if err != nil {
			return err
		}
	}

	return types.PrintResult(conf.PrevResult, conf.CNIVersion)
}

func cmdDel(args *skel.CmdArgs) error {
	conf, err := parseConfig(args.StdinData)
	if err != nil {
		return err
	}

	ifbDeviceName, err := getIfbDeviceName(conf.Name, args.ContainerID)
	if err != nil {
		return err
	}

	if err := TeardownIfb(ifbDeviceName); err != nil {
		return err
	}

	return nil
}

func main() {
	skel.PluginMain(cmdAdd, cmdDel, version.PluginSupports("0.3.0", "0.3.1", version.Current()))
}
