//  Copyright 2018 Google Inc. All Rights Reserved.
//
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the License.
//  You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
//  Unless required by applicable law or agreed to in writing, software
//  distributed under the License is distributed on an "AS IS" BASIS,
//  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//  See the License for the specific language governing permissions and
//  limitations under the License.

// Package config stores and retrieves configuration settings for the OS Config agent.
package config

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/compute/metadata"
	"golang.org/x/oauth2/jws"
)

const (
	// InstanceMetadata is the instance metadata URL.
	InstanceMetadata = "http://metadata.google.internal/computeMetadata/v1/instance"
	// IdentityTokenPath is the instance identity token path.
	IdentityTokenPath = "instance/service-accounts/default/identity?audience=osconfig.googleapis.com&format=full"
	// ReportURL is the guest attributes endpoint.
	ReportURL = InstanceMetadata + "/guest-attributes"

	googetRepoFilePath = "C:/ProgramData/GooGet/repos/google_osconfig_managed.repo"
	zypperRepoFilePath = "/etc/zypp/repos.d/google_osconfig_managed.repo"
	yumRepoFilePath    = "/etc/yum.repos.d/google_osconfig_managed.repo"
	aptRepoFilePath    = "/etc/apt/sources.list.d/google_osconfig_managed.list"

	prodEndpoint = "osconfig.googleapis.com:443"

	osInventoryEnabledDefault      = false
	guestPoliciesEnabledDefault    = false
	taskNotificationEnabledDefault = false
	debugEnabledDefault            = false

	configDirWindows     = `C:\Program Files\Google\OSConfig`
	configDirLinux       = "/etc/osconfig"
	taskStateFileWindows = configDirWindows + `\osconfig_task.state`
	taskStateFileLinux   = configDirLinux + "/osconfig_task.state"
	restartFileWindows   = configDirWindows + `\osconfig_agent_restart_required`
	restartFileLinux     = configDirLinux + "/osconfig_agent_restart_required"

	osConfigPollIntervalDefault = 10
)

var (
	endpoint = flag.String("endpoint", prodEndpoint, "osconfig endpoint override")
	debug    = flag.Bool("debug", false, "set debug log verbosity")
	stdout   = flag.Bool("stdout", false, "log to stdout")

	agentConfig   = &config{}
	agentConfigMx sync.RWMutex
	version       string
)

type config struct {
	osInventoryEnabled, guestPoliciesEnabled, taskNotificationEnabled, debugEnabled       bool
	svcEndpoint, googetRepoFilePath, zypperRepoFilePath, yumRepoFilePath, aptRepoFilePath string
	numericProjectID, osConfigPollInterval                                                int
	projectID, instanceZone, instanceName, instanceID                                     string
}

func (c *config) parseFeatures(features string, enabled bool) {
	for _, f := range strings.Split(features, ",") {
		f = strings.ToLower(strings.TrimSpace(f))
		switch f {
		case "tasks", "ospatch": // ospatch is the legacy flag
			c.taskNotificationEnabled = enabled
		case "guestpolicies", "ospackage": // ospackage is the legacy flag
			c.guestPoliciesEnabled = enabled
		case "osinventory":
			c.osInventoryEnabled = enabled
		}
	}
}

func getAgentConfig() config {
	agentConfigMx.RLock()
	defer agentConfigMx.RUnlock()
	return *agentConfig
}

func parseBool(s string) bool {
	enabled, err := strconv.ParseBool(s)
	if err != nil {
		// Bad entry returns as not enabled.
		return false
	}
	return enabled
}

type metadataJSON struct {
	Instance instanceJSON
	Project  projectJSON
}

type instanceJSON struct {
	Attributes attributesJSON
	Zone       string
	Name       string
	ID         *json.Number
}

type projectJSON struct {
	Attributes       attributesJSON
	ProjectID        string
	NumericProjectID int
}

type attributesJSON struct {
	InventoryEnabledOld   string       `json:"os-inventory-enabled"`
	InventoryEnabled      string       `json:"enable-os-inventory"`
	PreReleaseFeaturesOld string       `json:"os-config-enabled-prerelease-features"`
	PreReleaseFeatures    string       `json:"osconfig-enabled-prerelease-features"`
	OSConfigEnabled       string       `json:"enable-osconfig"`
	DisabledFeatures      string       `json:"osconfig-disabled-features"`
	DebugEnabledOld       string       `json:"enable-os-config-debug"`
	LogLevel              string       `json:"osconfig-log-level"`
	OSConfigEndpointOld   string       `json:"os-config-endpoint"`
	OSConfigEndpoint      string       `json:"osconfig-endpoint"`
	PollIntervalOld       *json.Number `json:"os-config-poll-interval"`
	PollInterval          *json.Number `json:"osconfig-poll-interval"`
}

func createConfigFromMetadata(md metadataJSON) *config {
	old := getAgentConfig()
	c := &config{
		osInventoryEnabled:      osInventoryEnabledDefault,
		guestPoliciesEnabled:    guestPoliciesEnabledDefault,
		taskNotificationEnabled: taskNotificationEnabledDefault,
		debugEnabled:            debugEnabledDefault,
		svcEndpoint:             prodEndpoint,
		osConfigPollInterval:    osConfigPollIntervalDefault,

		googetRepoFilePath: googetRepoFilePath,
		zypperRepoFilePath: zypperRepoFilePath,
		yumRepoFilePath:    yumRepoFilePath,
		aptRepoFilePath:    aptRepoFilePath,

		projectID:        old.projectID,
		numericProjectID: old.numericProjectID,
		instanceZone:     old.instanceZone,
		instanceName:     old.instanceName,
		instanceID:       old.instanceID,
	}

	if md.Project.ProjectID != "" {
		c.projectID = md.Project.ProjectID
	}
	if md.Project.NumericProjectID != 0 {
		c.numericProjectID = md.Project.NumericProjectID
	}
	if md.Instance.Zone != "" {
		c.instanceZone = md.Instance.Zone
	}
	if md.Instance.Name != "" {
		c.instanceName = md.Instance.Name
	}
	if md.Instance.ID != nil {
		c.instanceID = md.Instance.ID.String()
	}

	// Check project first then instance as instance metadata overrides project.
	switch {
	case md.Project.Attributes.InventoryEnabled != "":
		c.osInventoryEnabled = parseBool(md.Project.Attributes.InventoryEnabled)
	case md.Project.Attributes.InventoryEnabledOld != "":
		c.osInventoryEnabled = parseBool(md.Project.Attributes.InventoryEnabledOld)
	}

	c.parseFeatures(md.Project.Attributes.PreReleaseFeaturesOld, true)
	c.parseFeatures(md.Project.Attributes.PreReleaseFeatures, true)
	if md.Project.Attributes.OSConfigEnabled != "" {
		e := parseBool(md.Project.Attributes.OSConfigEnabled)
		c.taskNotificationEnabled = e
		c.guestPoliciesEnabled = e
		c.osInventoryEnabled = e
	}
	c.parseFeatures(md.Project.Attributes.DisabledFeatures, false)

	switch {
	case md.Instance.Attributes.InventoryEnabled != "":
		c.osInventoryEnabled = parseBool(md.Instance.Attributes.InventoryEnabled)
	case md.Instance.Attributes.InventoryEnabledOld != "":
		c.osInventoryEnabled = parseBool(md.Instance.Attributes.InventoryEnabledOld)
	}

	c.parseFeatures(md.Instance.Attributes.PreReleaseFeaturesOld, true)
	c.parseFeatures(md.Instance.Attributes.PreReleaseFeatures, true)
	if md.Instance.Attributes.OSConfigEnabled != "" {
		e := parseBool(md.Instance.Attributes.OSConfigEnabled)
		c.taskNotificationEnabled = e
		c.guestPoliciesEnabled = e
		c.osInventoryEnabled = e
	}
	c.parseFeatures(md.Instance.Attributes.DisabledFeatures, false)

	switch {
	case md.Project.Attributes.PollInterval != nil:
		if val, err := md.Project.Attributes.PollInterval.Int64(); err == nil {
			c.osConfigPollInterval = int(val)
		}
	case md.Project.Attributes.PollIntervalOld != nil:
		if val, err := md.Project.Attributes.PollIntervalOld.Int64(); err == nil {
			c.osConfigPollInterval = int(val)
		}
	}

	switch {
	case md.Instance.Attributes.PollInterval != nil:
		if val, err := md.Instance.Attributes.PollInterval.Int64(); err == nil {
			c.osConfigPollInterval = int(val)
		}
	case md.Instance.Attributes.PollIntervalOld != nil:
		if val, err := md.Instance.Attributes.PollInterval.Int64(); err == nil {
			c.osConfigPollInterval = int(val)
		}
	}

	switch {
	case md.Project.Attributes.DebugEnabledOld != "":
		c.debugEnabled = parseBool(md.Project.Attributes.DebugEnabledOld)
	case md.Instance.Attributes.DebugEnabledOld != "":
		c.debugEnabled = parseBool(md.Instance.Attributes.DebugEnabledOld)
	}

	switch strings.ToLower(md.Project.Attributes.LogLevel) {
	case "debug":
		c.debugEnabled = true
	case "info":
		c.debugEnabled = false
	}

	switch strings.ToLower(md.Instance.Attributes.LogLevel) {
	case "debug":
		c.debugEnabled = true
	case "info":
		c.debugEnabled = false
	}

	// Flags take precedence over metadata.
	if *debug {
		c.debugEnabled = true
	}

	switch {
	case *endpoint != prodEndpoint:
		c.svcEndpoint = *endpoint
	case md.Instance.Attributes.OSConfigEndpoint != "":
		c.svcEndpoint = md.Instance.Attributes.OSConfigEndpoint
	case md.Instance.Attributes.OSConfigEndpointOld != "":
		c.svcEndpoint = md.Instance.Attributes.OSConfigEndpointOld
	case md.Project.Attributes.OSConfigEndpoint != "":
		c.svcEndpoint = md.Project.Attributes.OSConfigEndpoint
	case md.Project.Attributes.OSConfigEndpointOld != "":
		c.svcEndpoint = md.Project.Attributes.OSConfigEndpointOld
	}

	return c
}

func formatMetadataError(err error) error {
	if urlErr, ok := err.(*url.Error); ok {
		if _, ok := urlErr.Err.(*net.DNSError); ok {
			return fmt.Errorf("DNS error when requesting metadata, check DNS settings and ensure metadata.google.internal is setup in your hosts file")
		}
		if _, ok := urlErr.Err.(*net.OpError); ok {
			return fmt.Errorf("network error when requesting metadata, make sure your instance has an active network and can reach the metadata server")
		}
	}
	return err
}

// SetConfig sets the agent config.
func SetConfig(ctx context.Context) error {
	var md string
	var webError error
	webErrorCount := 0
	ticker := time.NewTicker(5 * time.Second)
	for {
		md, webError = metadata.Get("?recursive=true&alt=json")
		if webError == nil {
			break
		}
		// Try up to 3 times to wait for slow network initialization, after
		// that resort to using defaults and returning the error.
		if webErrorCount == 2 {
			webError = formatMetadataError(webError)
			break
		}
		webErrorCount++
		select {
		case <-ticker.C:
			continue
		case <-ctx.Done():
			return nil
		}
	}

	var metadata metadataJSON
	if err := json.Unmarshal([]byte(md), &metadata); err != nil {
		return err
	}

	new := createConfigFromMetadata(metadata)
	agentConfigMx.Lock()
	agentConfig = new
	agentConfigMx.Unlock()

	return webError
}

// SvcPollInterval returns the frequency to poll the service.
func SvcPollInterval() time.Duration {
	return time.Duration(getAgentConfig().osConfigPollInterval) * time.Minute
}

// MaxMetadataRetryDelay is the maximum retry delay when getting data from the metadata server.
func MaxMetadataRetryDelay() time.Duration {
	return 30 * time.Second
}

// MaxMetadataRetries is the maximum number of retry when getting data from the metadata server.
func MaxMetadataRetries() int {
	return 3
}

// SerialLogPort is the serial port to log to.
func SerialLogPort() string {
	if runtime.GOOS == "windows" {
		return "COM1"
	}
	// Don't write directly to the serial port on Linux as syslog already writes there.
	return ""
}

// Debug sets the debug log verbosity.
func Debug() bool {
	return (*debug || getAgentConfig().debugEnabled)
}

// Stdout flag.
func Stdout() bool {
	return *stdout
}

// SvcEndpoint is the OS Config service endpoint.
func SvcEndpoint() string {
	return getAgentConfig().svcEndpoint
}

// ZypperRepoFilePath is the location where the zypper repo file will be created.
func ZypperRepoFilePath() string {
	return getAgentConfig().zypperRepoFilePath
}

// YumRepoFilePath is the location where the zypper repo file will be created.
func YumRepoFilePath() string {
	return getAgentConfig().yumRepoFilePath
}

// AptRepoFilePath is the location where the zypper repo file will be created.
func AptRepoFilePath() string {
	return getAgentConfig().aptRepoFilePath
}

// GooGetRepoFilePath is the location where the googet repo file will be created.
func GooGetRepoFilePath() string {
	return getAgentConfig().googetRepoFilePath
}

// OSInventoryEnabled indicates whether OSInventory should be enabled.
func OSInventoryEnabled() bool {
	return getAgentConfig().osInventoryEnabled
}

// GuestPoliciesEnabled indicates whether GuestPolicies should be enabled.
func GuestPoliciesEnabled() bool {
	return getAgentConfig().guestPoliciesEnabled
}

// TaskNotificationEnabled indicates whether TaskNotification should be enabled.
func TaskNotificationEnabled() bool {
	return getAgentConfig().taskNotificationEnabled
}

// Instance is the URI of the instance the agent is running on.
func Instance() string {
	// Zone contains 'projects/project-id/zones' as a prefix.
	return fmt.Sprintf("%s/instances/%s", Zone(), Name())
}

// NumericProjectID is the numeric project ID of the instance.
func NumericProjectID() int {
	return getAgentConfig().numericProjectID
}

// ProjectID is the project ID of the instance.
func ProjectID() string {
	return getAgentConfig().projectID
}

// Zone is the zone the instance is running in.
func Zone() string {
	return getAgentConfig().instanceZone
}

// Name is the instance name.
func Name() string {
	return getAgentConfig().instanceName
}

// ID is the instance id.
func ID() string {
	return getAgentConfig().instanceID
}

type idToken struct {
	raw string
	exp *time.Time
	sync.Mutex
}

func (t *idToken) get() error {
	data, err := metadata.Get(IdentityTokenPath)
	if err != nil {
		return fmt.Errorf("error getting token from metadata: %v", err)
	}

	cs, err := jws.Decode(data)
	if err != nil {
		return err
	}

	t.raw = data
	exp := time.Unix(cs.Exp, 0)
	t.exp = &exp

	return nil
}

var identity idToken

// IDToken is the instance id token.
func IDToken() (string, error) {
	identity.Lock()
	defer identity.Unlock()

	// Rerequest token if expiry is within 10 minutes.
	if identity.exp == nil || time.Now().After(identity.exp.Add(-10*time.Minute)) {
		if err := identity.get(); err != nil {
			return "", err
		}
	}

	return identity.raw, nil
}

// Version is the agent version.
func Version() string {
	return version
}

// SetVersion sets the agent version.
func SetVersion(v string) {
	version = v
}

// TaskStateFile is the location of the task state file.
func TaskStateFile() string {
	if runtime.GOOS == "windows" {
		return taskStateFileWindows
	}

	return taskStateFileLinux
}

// RestartFile is the location of the restart required file.
func RestartFile() string {
	if runtime.GOOS == "windows" {
		return restartFileWindows
	}

	return restartFileLinux
}
