package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"
	"github.com/vishvananda/netlink"

	"github.com/containernetworking/plugins/pkg/dan"
	"github.com/containernetworking/plugins/pkg/ip"
	"github.com/containernetworking/plugins/pkg/ipam"
	"github.com/containernetworking/plugins/pkg/ns"
	bv "github.com/containernetworking/plugins/pkg/utils/buildversion"
)

type NetConf struct {
	types.NetConf
	Bridge   string `json:"bridge"`
	BridgeIP string `json:"bridgeIP"`
}

func loadNetConf(bytes []byte) (*NetConf, error) {
	conf := &NetConf{}
	if err := json.Unmarshal(bytes, conf); err != nil {
		return nil, fmt.Errorf("failed to parse network config: %v", err)
	}
	return conf, nil
}

// Create a tap interface
func createTapInterface(conf *NetConf, ifName string) (*current.Interface, error) {
	tapInterface := &current.Interface{}

	br, err := netlink.LinkByName(conf.Bridge)
	if err != nil {
		// Create a bridge if not exists
		if _, ok := err.(netlink.LinkNotFoundError); ok {
			// setup bridge
			// https://gist.github.com/extremecoders-re/e8fd8a67a515fee0c873dcafc81d811c?permalink_comment_id=4039841#gistcomment-4039841
			// https://krackout.wordpress.com/2020/03/08/network-bridges-and-tun-tap-interfaces-in-linux/

			br = &netlink.Bridge{
				LinkAttrs: netlink.LinkAttrs{
					Name: conf.Bridge,
				},
			}

			if err := netlink.LinkAdd(br); err != nil {
				return nil, fmt.Errorf("failed to create bridge link: %v", err)
			}

			_, ipv4Net, err := net.ParseCIDR(conf.BridgeIP)
			if err != nil {
				return nil, fmt.Errorf("failed to parse bridge IP(%+v): %v", conf.BridgeIP, err)
			}

			addr := &netlink.Addr{IPNet: ipv4Net, Label: ""}
			if err = netlink.AddrAdd(br, addr); err != nil {
				return nil, fmt.Errorf("failed to add IP addr %v to %q: %v", ipv4Net, conf.Bridge, err)
			}
		} else {
			return nil, fmt.Errorf("failed to fetch master bridge device %q: %v", conf.Bridge, err)
		}
	}
	dan.Log("Bridge = %+v", br)

	tap := &netlink.Tuntap{
		LinkAttrs: netlink.LinkAttrs{
			Name: ifName,
		},
		Mode: netlink.TUNTAP_MODE_TAP,
	}

	if err := netlink.LinkAdd(tap); err != nil {
		return nil, fmt.Errorf("failed to create tap link: %v", err)
	}
	tapInterface.Name = ifName

	// set master: `ip link set $link master $master`
	if err := netlink.LinkSetMaster(tap, br); err != nil {
		return nil, fmt.Errorf("failed to link tap device %q to master %+v: %v", ifName, br, err)
	}

	// Re-fetch interface to get all properties/attributes
	tapGot, err := netlink.LinkByName(ifName)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch tap device %q: %v", ifName, err)
	}
	dan.Log("A tap interface was created, tap = %+v", tapGot)

	tapInterface.Mac = tapGot.Attrs().HardwareAddr.String()

	if err = netlink.LinkSetUp(tapGot); err != nil {
		return nil, fmt.Errorf("failed to set %+v up: %v", tapGot, err)
	}
	return tapInterface, nil
}

func cmdAdd(args *skel.CmdArgs) error {
	dan.Log(">>>>>>>> DanTap cmdAdd >>>>>>>>>>>>")
	defer dan.Log(">>>>>>>> DanTap cmdAdd >>>>>>>>>>>>")
	dan.Log("args %+v", args)

	conf, err := loadNetConf(args.StdinData)
	if err != nil {
		return err
	}
	dan.Log("conf %+v", conf)

	if conf.IPAM.Type == "" {
		return errors.New("tap interface requires an IPAM configuration")
	}

	// FIXME now use fixed dan0 as host side interface
	hostTapName := "dan0"
	tapInterface, err := createTapInterface(conf, hostTapName)
	if err != nil {
		return err
	}

	result := &current.Result{}
	defer func() {
		danIns := dan.New(hostTapName, dan.TapType, result)
		_ = danIns.Save(args.ContainerID)
	}()

	// Delete link if err to avoid link leak in this ns
	defer func() {
		if err != nil {
			err = ip.DelLinkByName(hostTapName)
		}
	}()

	r, err := ipam.ExecAdd(conf.IPAM.Type, args.StdinData)
	if err != nil {
		return err
	}

	// defer ipam deletion to avoid ip leak
	defer func() {
		if err != nil {
			ipam.ExecDel(conf.IPAM.Type, args.StdinData)
		}
	}()

	// convert IPAMResult to current Result type
	result, err = current.NewResultFromResult(r)
	if err != nil {
		return err
	}

	if len(result.IPs) == 0 {
		return errors.New("IPAM plugin returned missing IP config")
	}

	for _, ipc := range result.IPs {
		// all addresses apply to the container tap interface
		ipc.Interface = current.Int(0)
	}

	result.Interfaces = []*current.Interface{tapInterface}
	dan.Log("Result %+v", result)

	return types.PrintResult(result, conf.CNIVersion)
}

func cmdDel(args *skel.CmdArgs) error {
	dan.Log(">>>>>>>> DanTap cmdDel >>>>>>>>>>>>")
	defer dan.Log(">>>>>>>> DanTap cmdDel >>>>>>>>>>>>")
	defer dan.Destory(args.ContainerID)
	conf, err := loadNetConf(args.StdinData)
	if err != nil {
		return err
	}
	dan.Log("cmdDel conf %+v", conf)

	if err = ipam.ExecDel(conf.IPAM.Type, args.StdinData); err != nil {
		dan.Log("cmdDel ipam.ExecDel error %+v", err)
		return err
	}

	// FIXME dan0
	err = ip.DelLinkByName("dan0")
	if err != nil && err == ip.ErrLinkNotFound {
		dan.Log(" cmdDel ip.DelLinkByName not found %+v", err)
		return nil
	}
	dan.Log("cmdDel dan0 deleted %+v", err)

	if err != nil {
		//  if NetNs is passed down by the Cloud Orchestration Engine, or if it called multiple times
		// so don't return an error if the device is already removed.
		// https://github.com/kubernetes/kubernetes/issues/43014#issuecomment-287164444
		_, ok := err.(ns.NSPathNotExistErr)
		if ok {
			return nil
		}
		return err
	}

	return nil
}

func main() {
	skel.PluginMain(cmdAdd, cmdCheck, cmdDel, version.All, bv.BuildString("tap"))
}

func cmdCheck(args *skel.CmdArgs) error {
	conf, err := loadNetConf(args.StdinData)
	if err != nil {
		return err
	}
	dan.Log("cmdCheck conf %+v", conf)

	return nil
}
