package main

import "os"

func terminateServiceProcess(code int) {
	os.Exit(code)
}
