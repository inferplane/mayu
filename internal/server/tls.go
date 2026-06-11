package server

import "errors"

// ValidateTLS checks the TLS file pair is fully specified (both or neither).
func ValidateTLS(certFile, keyFile string) error {
	if (certFile == "") != (keyFile == "") {
		return errors.New("server.tls: cert_file and key_file must both be set or both empty")
	}
	return nil
}
