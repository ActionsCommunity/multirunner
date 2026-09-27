package backend

import (
	"strings"
	"testing"

	"github.com/docker/docker/client"
)

func TestDockerTLSClientOptionDisabledWithoutConfig(t *testing.T) {
	opt, err := dockerTLSClientOption(DockerTLSConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if opt != nil {
		t.Fatal("TLS option applied to a non-matching Docker daemon")
	}
}

func TestDockerTLSClientOptionRequiresCompletePaths(t *testing.T) {
	_, err := dockerTLSClientOption(DockerTLSConfig{CAFile: "ca.pem", KeyFile: "key.pem"})
	if err == nil {
		t.Fatal("incomplete Docker TLS configuration was accepted")
	}
	for _, name := range []string{"CA", "certificate", "key"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not name %s", err, name)
		}
	}
}

func TestDockerTLSClientOptionLoadsCertificates(t *testing.T) {
	opt, err := dockerTLSClientOption(DockerTLSConfig{
		CAFile: "missing-ca.pem", CertFile: "missing-cert.pem", KeyFile: "missing-key.pem",
	})
	if err != nil {
		t.Fatal(err)
	}
	if opt == nil {
		t.Fatal("matching Docker TLS host did not produce a client option")
	}
	if _, err := client.NewClientWithOpts(opt); err == nil {
		t.Fatal("Docker TLS option did not attempt to load the configured certificates")
	}
}

func TestTLSBackendConstructorsRejectMissingCertificates(t *testing.T) {
	tls := DockerTLSConfig{
		CAFile: "missing-ca.pem", CertFile: "missing-cert.pem", KeyFile: "missing-key.pem",
	}
	if _, err := NewDockerLinuxTLS("tcp://127.0.0.1:2376", tls); err == nil {
		t.Fatal("Linux TLS backend accepted missing certificate files")
	}
	if _, err := NewDockerWindowsTLS("tcp://127.0.0.1:2376", "process", tls); err == nil {
		t.Fatal("Windows TLS backend accepted missing certificate files")
	}
}
