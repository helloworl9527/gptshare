package config

import "os"

func lookupEnv(name string) (string, bool) {
	return os.LookupEnv(name)
}
