package sysutil

// DiskFreeGB returns free disk space in GB for the given path.
// On Windows this is a stub returning 0 (not used in production).
func DiskFreeGB(path string) (float64, error) {
	return 0, nil
}