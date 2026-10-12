package guest

import "errors"

func setHostname(string) error { return errors.New("guest hostname requires Linux") }
