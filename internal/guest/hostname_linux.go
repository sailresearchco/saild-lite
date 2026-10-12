package guest

import "golang.org/x/sys/unix"

func setHostname(name string) error { return unix.Sethostname([]byte(name)) }
