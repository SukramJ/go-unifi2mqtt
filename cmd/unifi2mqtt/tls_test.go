// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SukramJ/go-unifi2mqtt/internal/config"
)

// writeCA writes a freshly generated self-signed CA certificate as PEM
// and returns its path.
func writeCA(t *testing.T) string {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "probe broker CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	path := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write CA: %v", err)
	}
	return path
}

func loadCfg(t *testing.T, yaml string) *config.Config {
	t.Helper()

	cfg, err := config.Load(strings.NewReader("HOST: h\nAPI_KEY: k\nMQTT_SERVER: broker.example\n"+yaml), config.MapEnv{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

// The broker connection's TLS settings come from MQTT_TLS,
// MQTT_TLS_INSECURE and MQTT_TLS_CA, and the server name is the bare
// MQTT_SERVER host — the name the certificate has to carry.
func TestMQTTTLSConfig(t *testing.T) {
	t.Parallel()

	t.Run("off", func(t *testing.T) {
		t.Parallel()

		cfg := loadCfg(t, "")
		tc, err := mqttTLSConfig(cfg)
		if err != nil || tc != nil {
			t.Errorf("mqttTLSConfig = %v, %v; want nil, nil without MQTT_TLS", tc, err)
		}
		if got := cfg.MQTTBrokerURL(); got != "tcp://broker.example:1883" {
			t.Errorf("MQTTBrokerURL() = %q, want the tcp scheme", got)
		}
	})

	t.Run("system roots", func(t *testing.T) {
		t.Parallel()

		cfg := loadCfg(t, "MQTT_TLS: true\n")
		tc, err := mqttTLSConfig(cfg)
		if err != nil {
			t.Fatalf("mqttTLSConfig: %v", err)
		}
		if tc.ServerName != "broker.example" {
			t.Errorf("ServerName = %q, want the bare broker host", tc.ServerName)
		}
		if tc.InsecureSkipVerify {
			t.Error("InsecureSkipVerify on by default")
		}
		if tc.RootCAs != nil {
			t.Error("RootCAs set without MQTT_TLS_CA, want the system roots")
		}
		if got := cfg.MQTTBrokerURL(); got != "tls://broker.example:8883" {
			t.Errorf("MQTTBrokerURL() = %q, want the tls scheme on 8883", got)
		}
	})

	t.Run("insecure", func(t *testing.T) {
		t.Parallel()

		tc, err := mqttTLSConfig(loadCfg(t, "MQTT_TLS: true\nMQTT_TLS_INSECURE: true\n"))
		if err != nil {
			t.Fatalf("mqttTLSConfig: %v", err)
		}
		if !tc.InsecureSkipVerify {
			t.Error("InsecureSkipVerify off, want the explicit opt-in honoured")
		}
	})

	t.Run("private CA", func(t *testing.T) {
		t.Parallel()

		ca := writeCA(t)
		tc, err := mqttTLSConfig(loadCfg(t, "MQTT_TLS: true\nMQTT_TLS_CA: "+ca+"\n"))
		if err != nil {
			t.Fatalf("mqttTLSConfig: %v", err)
		}
		if tc.RootCAs == nil {
			t.Fatal("RootCAs nil, want the MQTT_TLS_CA bundle")
		}
		if tc.InsecureSkipVerify {
			t.Error("InsecureSkipVerify on with a CA")
		}
		if tc.ServerName != "broker.example" {
			t.Errorf("ServerName = %q, want the bare broker host", tc.ServerName)
		}
	})

	t.Run("unusable CA names the file", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		notPEM := filepath.Join(dir, "not-a-cert.pem")
		if err := os.WriteFile(notPEM, []byte("hello"), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{filepath.Join(dir, "missing.pem"), notPEM} {
			_, err := mqttTLSConfig(loadCfg(t, "MQTT_TLS: true\nMQTT_TLS_CA: "+path+"\n"))
			if err == nil {
				t.Errorf("%s: accepted, want an error", path)
				continue
			}
			if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "MQTT_TLS_CA") {
				t.Errorf("error %q does not name MQTT_TLS_CA and %s", err, path)
			}
		}
	})
}
