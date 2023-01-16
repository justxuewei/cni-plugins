package dan

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"os"

	current "github.com/containernetworking/cni/pkg/types/100"
)

var baseDir = "/run/dans"

type Type string

const TapType Type = "tap"

type DirectlyAttachableNetwork struct {
	DevType     Type         `json:"type"`
	DevName     string       `json:"name"`
	NetworkInfo *NetworkInfo `json:"network_info"`
}

type NetworkInfo struct {
	Interface *Interface `json:"interface"`
	Routes    []*Route   `json:"routes"`
}

type Interface struct {
	IPAddresses []*IPAddress `json:"ip_addresses"`
}

type IPAddress struct {
	Family  string `json:"family"`
	Address string `json:"address"`
	Mask    string `json:"mask"`
}

type Route struct {
	Dest    string `json:"dest"`
	Gateway string `json:"gateway"`
	Family  string `json:"family"`
}

func New(devName string, devType Type, r *current.Result) *DirectlyAttachableNetwork {
	networkInfo := &NetworkInfo{}
	// IP addresses
	iface := &Interface{}
	var ipAddresses []*IPAddress
	for _, ip := range r.IPs {
		ipAddresses = append(ipAddresses, &IPAddress{
			Family:  "v4",
			Address: ip.Address.IP.String(),
			Mask:    fmt.Sprintf("%d", maskByteToInt(ip.Address.Mask)),
		})
	}
	iface.IPAddresses = ipAddresses
	networkInfo.Interface = iface
	// Routes
	var routes []*Route
	for _, route := range r.Routes {
		gateway := ""
		if len(route.GW) != 0 {
			gateway = route.GW.String()
		}
		routes = append(routes, &Route{
			Dest:    route.Dst.String(),
			Gateway: gateway,
			Family:  "v4",
		})
	}
	networkInfo.Routes = routes

	return &DirectlyAttachableNetwork{
		DevType:     devType,
		DevName:     devName,
		NetworkInfo: networkInfo,
	}
}

func (n *DirectlyAttachableNetwork) Save(containerID string) error {
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return err
	}

	var network = []*DirectlyAttachableNetwork{n}
	data, _ := json.Marshal(network)
	Log("Network info = %s", string(data))
	return os.WriteFile(fmt.Sprintf("%s/%s.json", baseDir, containerID), data, 0644)
}

func Destory(containerID string) {
	path := fmt.Sprintf("%s/%s.json", baseDir, containerID)
	if err := os.Remove(path); err != nil {
		Log("Failed to remove dan conf at %s, caused by %v", path, err)
	}
}

func Log(format string, args ...interface{}) {
	f, err := os.OpenFile("/tmp/dan.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return
	}

	defer f.Close()
	fmt.Fprintf(f, format+"\n", args...)
}

// Convert a mask encoded by a byte array to an int
func maskByteToInt(mask net.IPMask) int {
	var i uint32
    buf := bytes.NewReader(mask)
    _ = binary.Read(buf, binary.BigEndian, &i)

	Log("i = %x, mask = %s", i, mask.String())
    
	ret := 32
	for ; i>0; i >>= 1 {
		if i & 0x1 == 0x0 {
			ret -= 1
		} else {
			return ret
		}
	}

	return ret
}
