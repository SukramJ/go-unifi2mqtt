// SPDX-License-Identifier: MIT
// Copyright (C) 2026 SukramJ

package config

// Default values. These are applied before the YAML file is decoded, so
// a key absent from the file keeps its default, and an explicitly set
// key always wins — including when it sets the zero value.
//
// The defaults are chosen so a config supplying only HOST, API_KEY and
// MQTT_SERVER produces a working read-only bridge: clients and controls
// stay off, the console's self-signed certificate is tolerated, and the
// polling cadences are gentle enough not to bother the console.
const (
	DefaultPort        = 443
	DefaultSite        = "default"
	DefaultHTTPTimeout = 10
	DefaultHTTPRetries = 3

	DefaultMQTTPort    = 1883
	DefaultMQTTSSLPort = 8883
	DefaultMQTTTopic   = "unifi"

	DefaultHASSBaseTopic      = "homeassistant"
	DefaultHASSBirthGracetime = 15
	// DefaultHASSCleanup enables the orphan reconcile. On, because the
	// failure it prevents is silent: a config left behind by an older
	// version recreates an entity that can never receive a value, and
	// nothing in Home Assistant says where it came from.
	DefaultHASSCleanup = true

	DefaultRefreshDevices = 60
	DefaultRefreshClients = 30
	DefaultRefreshHealth  = 60
	DefaultRefreshStatic  = 3600
	DefaultForceRepublish = 600

	DefaultClientsMax         = 100
	DefaultClientsAwayTimeout = 300

	DefaultWebBind  = "127.0.0.1:8080"
	DefaultLanguage = "en"

	// MinRefreshSeconds is the floor every poll cadence is validated
	// against. Anything faster risks tripping the console's rate limit
	// without producing meaningfully fresher data.
	MinRefreshSeconds = 5
)

// defaults returns a Config pre-populated with the values above.
func defaults() Config {
	return Config{
		Port:        DefaultPort,
		Site:        DefaultSite,
		HTTPTimeout: DefaultHTTPTimeout,
		HTTPRetries: DefaultHTTPRetries,

		MQTTPort:  DefaultMQTTPort,
		MQTTTopic: DefaultMQTTTopic,

		HASSBaseTopic:      DefaultHASSBaseTopic,
		HASSBirthGracetime: DefaultHASSBirthGracetime,
		HASSCleanup:        DefaultHASSCleanup,

		RefreshDevices: DefaultRefreshDevices,
		RefreshClients: DefaultRefreshClients,
		RefreshHealth:  DefaultRefreshHealth,
		RefreshStatic:  DefaultRefreshStatic,
		ForceRepublish: DefaultForceRepublish,

		Clients: ClientsConfig{
			// Wireless-only is the useful default for presence
			// detection: wired clients are usually stationary, so a
			// device_tracker for them carries no information.
			Types:         []string{"WIRELESS"},
			ExcludeGuests: true,
			Max:           DefaultClientsMax,
			AwayTimeout:   DefaultClientsAwayTimeout,
		},

		Controls: ControlsConfig{
			// Which controls are on *if* CONTROLS.ENABLE is flipped.
			// The three classic-only ones stay off so enabling controls
			// on an official-API-only setup does not immediately trip
			// the validation.
			DeviceRestart:  true,
			PortPowerCycle: true,
			GuestAuthorize: true,
		},

		WebBind:  DefaultWebBind,
		Language: DefaultLanguage,
	}
}
