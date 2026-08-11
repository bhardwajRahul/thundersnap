package thunderboot

import (
	"fmt"
	"net/url"
	"strings"
)

// DiskSpec describes either one block device, an md RAID made from several
// devices, or (for backing storage only) an NBD export.
type DiskSpec struct {
	RAID    string
	Devices []string
	NBD     *url.URL
}

func ParseDiskSpec(s string, allowNBD bool) (DiskSpec, error) {
	var spec DiskSpec
	if s == "" {
		return spec, nil
	}
	if strings.HasPrefix(s, "nbd://") {
		if !allowNBD {
			return spec, fmt.Errorf("NBD is not valid for cache storage")
		}
		u, err := url.Parse(s)
		if err != nil || u.Host == "" {
			return spec, fmt.Errorf("invalid NBD URL %q", s)
		}
		spec.NBD = u
		return spec, nil
	}
	devices := s
	if raid, rest, ok := strings.Cut(s, ";"); ok {
		if raid != "raid0" && raid != "raid1" {
			return spec, fmt.Errorf("unsupported RAID level %q", raid)
		}
		spec.RAID, devices = raid, rest
	}
	for _, device := range strings.Split(devices, ",") {
		if device == "" {
			return spec, fmt.Errorf("empty device in %q", s)
		}
		spec.Devices = append(spec.Devices, device)
	}
	if spec.RAID != "" && len(spec.Devices) < 2 {
		return spec, fmt.Errorf("%s requires at least two devices", spec.RAID)
	}
	return spec, nil
}

func (s DiskSpec) String() string {
	if s.NBD != nil {
		return s.NBD.String()
	}
	prefix := ""
	if s.RAID != "" {
		prefix = s.RAID + ";"
	}
	return prefix + strings.Join(s.Devices, ",")
}
