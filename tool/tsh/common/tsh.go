/*
 * Teleport
 * Copyright (C) 2023  Gravitational, Inc.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package common

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/pprof"
	runtimetrace "runtime/trace"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"github.com/dustin/go-humanize"
	"github.com/ghodss/yaml"
	"github.com/google/uuid"
	"github.com/gravitational/trace"
	"github.com/jonboulle/clockwork"
	"go.opentelemetry.io/otel/attribute"
	oteltrace "go.opentelemetry.io/otel/trace"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"

	"github.com/gravitational/teleport"
	apiclient "github.com/gravitational/teleport/api/client"
	"github.com/gravitational/teleport/api/client/proto"
	"github.com/gravitational/teleport/api/constants"
	apidefaults "github.com/gravitational/teleport/api/defaults"
	"github.com/gravitational/teleport/api/metadata"
	"github.com/gravitational/teleport/api/profile"
	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/api/types/accesslist"
	apievents "github.com/gravitational/teleport/api/types/events"
	"github.com/gravitational/teleport/api/types/wrappers"
	apiutils "github.com/gravitational/teleport/api/utils"
	"github.com/gravitational/teleport/api/utils/keys/hardwarekey"
	"github.com/gravitational/teleport/api/utils/keys/piv"
	"github.com/gravitational/teleport/api/utils/prompt"
	"github.com/gravitational/teleport/lib/asciitable"
	"github.com/gravitational/teleport/lib/auth/authclient"
	wancli "github.com/gravitational/teleport/lib/auth/webauthncli"
	"github.com/gravitational/teleport/lib/autoupdate/tools"
	"github.com/gravitational/teleport/lib/benchmark"
	benchmarkdb "github.com/gravitational/teleport/lib/benchmark/db"
	"github.com/gravitational/teleport/lib/client"
	dbprofile "github.com/gravitational/teleport/lib/client/db"
	dbmcp "github.com/gravitational/teleport/lib/client/db/mcp"
	"github.com/gravitational/teleport/lib/client/identityfile"
	"github.com/gravitational/teleport/lib/client/reexec"
	"github.com/gravitational/teleport/lib/defaults"
	dtauthn "github.com/gravitational/teleport/lib/devicetrust/authn"
	dtenroll "github.com/gravitational/teleport/lib/devicetrust/enroll"
	libhwk "github.com/gravitational/teleport/lib/hardwarekey"
	"github.com/gravitational/teleport/lib/kube/kubeconfig"
	"github.com/gravitational/teleport/lib/modules"
	"github.com/gravitational/teleport/lib/observability/tracing"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/session"
	"github.com/gravitational/teleport/lib/shell"
	"github.com/gravitational/teleport/lib/sshutils/sftp"
	"github.com/gravitational/teleport/lib/sshutils/x11"
	"github.com/gravitational/teleport/lib/tlsca"
	"github.com/gravitational/teleport/lib/utils"
	"github.com/gravitational/teleport/lib/utils/diagnostics/latency"
	logutils "github.com/gravitational/teleport/lib/utils/log"
	"github.com/gravitational/teleport/lib/utils/mlock"
	stacksignal "github.com/gravitational/teleport/lib/utils/signal"
	"github.com/gravitational/teleport/tool/common"
	"github.com/gravitational/teleport/tool/common/fido2"
	"github.com/gravitational/teleport/tool/common/touchid"
	"github.com/gravitational/teleport/tool/common/webauthnwin"
)

var (
	logger = logutils.NewPackageLogger(teleport.ComponentKey, teleport.ComponentTSH)
)

const (
	// mfaModeAuto automatically chooses the best MFA device(s), without any
	// restrictions.
	// Allows both Webauthn and OTP.
	mfaModeAuto = "auto"
	// mfaModeCrossPlatform utilizes only cross-platform devices, such as
	// pluggable hardware keys.
	// Implies Webauthn.
	mfaModeCrossPlatform = "cross-platform"
	// mfaModePlatform utilizes only platform devices, such as Touch ID.
	// Implies Webauthn.
	mfaModePlatform = "platform"
	// mfaModeOTP utilizes only OTP devices.
	mfaModeOTP = "otp"
	// mfaModeSSO utilizes only SSO devices.
	mfaModeSSO = "sso"
)

const (
	// accessRequestModeOff disables automatic access requests.
	accessRequestModeOff = "off"
	// accessRequestModeResource enables automatic resource access requests.
	accessRequestModeResource = "resource"
	// accessRequestModeRole enables automatic role access requests.
	accessRequestModeRole = "role"
)

var accessRequestModes = []string{
	accessRequestModeOff,
	accessRequestModeResource,
	accessRequestModeRole,
}

// ClientInitFunc defines a function that initiates a connection to
// the Teleport cluster using the CLI configuration.
type ClientInitFunc func(cf *CLIConf) (*client.TeleportClient, error)

// CLIConf stores command line arguments and flags:
type CLIConf struct {
	// UserHost contains "[login]@hostname" argument to SSH command
	UserHost string
	// Commands to execute on a remote host
	RemoteCommand []string
	// DesiredRoles indicates one or more roles which should be requested.
	DesiredRoles string
	// RequestReason indicates the reason for an access request.
	RequestReason string
	// SuggestedReviewers is a list of suggested request reviewers.
	SuggestedReviewers string
	// NoWait can be used with an access request to exit without waiting for a request resolution.
	NoWait bool
	// RequestedResourceIDs is a list of resources to request access to.
	RequestedResourceIDs []string
	// RequestID is an access request ID
	RequestID string
	// RequestIDs is a list of access request IDs
	RequestIDs []string
	// RequestMode is the type of access request to automatically make if needed.
	RequestMode string
	// ReviewReason indicates the reason for an access review.
	ReviewReason string
	// ReviewableRequests indicates that only requests which can be reviewed should
	// be listed.
	ReviewableRequests bool
	// SuggestedRequests indicates that only requests which suggest the current user
	// as a reviewer should be listed.
	SuggestedRequests bool
	// MyRequests indicates that only requests created by the current user
	// should be listed.
	MyRequests bool
	// Approve/Deny indicates the desired review kind.
	Approve, Deny bool
	// AssumeStartTimeRaw format is RFC3339
	AssumeStartTimeRaw string
	// ResourceKind is the resource kind to search for
	ResourceKind string
	// Username is the Teleport user's username (to login into proxies)
	Username string
	// ExplicitUsername is true if Username was initially set by the end-user
	// (for example, using command-line flags).
	ExplicitUsername bool
	// Proxy keeps the hostname:port of the Teleport proxy to use
	Proxy string
	// TTL defines how long a session must be active (in minutes)
	MinsToLive int32
	// SSH Port on a remote SSH host
	NodePort int32
	// Login on a remote SSH host
	NodeLogin string
	// InsecureSkipVerify bypasses verification of HTTPS certificate when talking to web proxy
	InsecureSkipVerify bool
	// SessionID identifies the session tsh is operating on.
	// For `tsh join`, it is the ID of the session to join.
	// For `tsh play`, it is either the ID of the session to play,
	// or the path to a local session file which has already been
	// downloaded.
	SessionID string
	// Src:dest parameter for SCP
	CopySpec []string
	// -r flag for scp
	RecursiveCopy bool
	// -L flag for ssh. Local port forwarding like 'ssh -L 80:remote.host:80 -L 443:remote.host:443'
	LocalForwardPorts []string
	// DynamicForwardedPorts is port forwarding using SOCKS5. It is similar to
	// "ssh -D 8080 example.com".
	DynamicForwardedPorts []string
	// -R flag for ssh. Remote port forwarding like 'ssh -R 80:localhost:80 -R 443:localhost:443'
	RemoteForwardPorts []string
	// ForwardAgent agent to target node. Equivalent of -A for OpenSSH.
	ForwardAgent bool
	// ProxyJump is an optional -J flag pointing to the list of jumphosts,
	// it is an equivalent of --proxy flag in tsh interpretation
	ProxyJump string
	// --local flag for ssh
	LocalExec bool
	// SiteName specifies remote site to login to.
	SiteName string
	// KubernetesCluster specifies the kubernetes cluster to login to.
	KubernetesCluster string

	// DaemonAddr is the daemon listening address.
	DaemonAddr string
	// DaemonCertsDir is the directory containing certs used to create secure gRPC connection with daemon service
	DaemonCertsDir string
	// DaemonPrehogAddr is the URL where prehog events should be submitted.
	DaemonPrehogAddr string
	// DaemonKubeconfigsDir is the directory "Directory containing kubeconfig
	// for Kubernetes Access.
	DaemonKubeconfigsDir string
	// DaemonAgentsDir contains agent config files and data directories for Connect My Computer.
	DaemonAgentsDir string
	// DaemonPid is the PID to be stopped by tsh daemon stop.
	DaemonPid int
	// DaemonInstallationID is a unique ID identifying a specific Teleport Connect installation.
	DaemonInstallationID string

	// DatabaseService specifies the database proxy server to log into.
	DatabaseService string
	// DatabaseServices specifies a list of database services.
	DatabaseServices string
	// DatabaseUser specifies database user to embed in the certificate.
	DatabaseUser string
	// DatabaseName specifies database name to embed in the certificate.
	DatabaseName string
	// DatabaseRoles specifies database roles to embed in the certificate.
	DatabaseRoles string
	// DatabaseCommand specifies the command to execute.
	DatabaseCommand string

	// AppName specifies proxied application name.
	AppName string
	// Interactive sessions will allocate a PTY and create interactive "shell"
	// sessions.
	Interactive bool
	// NonInteractive sessions will not allocate a PTY and create
	// non-interactive "exec" sessions. This variable is needed due to
	// limitations in kingpin (lack of an inverse short flag) which forces
	// the registration of both flags.
	NonInteractive bool
	// ShowVersion is an OpenSSH compatibility flag that prints out the version
	// of tsh. Useful for users that alias ssh to "tsh ssh".
	ShowVersion bool
	// Quiet mode, -q command (disables progress printing)
	Quiet bool
	// NoCache is used to turn off client cache for nodes discovery
	NoCache bool
	// BenchDuration is a duration for the benchmark
	BenchDuration time.Duration
	// BenchRate is a requests per second rate to maintain
	BenchRate int
	// BenchInteractive indicates that we should create interactive session
	BenchInteractive bool
	// BenchRandom indicates that we should connect to a random host each time
	BenchRandom bool
	// BenchExport exports the latency profile
	BenchExport bool
	// BenchExportPath saves the latency profile in provided path
	BenchExportPath string
	// BenchMaxSessions is the maximum number of sessions to open
	BenchMaxSessions int
	// BenchTicks ticks per half distance
	BenchTicks int32
	// BenchValueScale value at which to scale the values recorded
	BenchValueScale float64
	// Context is a context to control execution
	Context context.Context
	// IdentityFileIn is an argument to -i flag (path to the private key+cert file)
	IdentityFileIn string
	// Compatibility flags, --compat, specifies OpenSSH compatibility flags.
	Compatibility string
	// CertificateFormat defines the format of the user SSH certificate.
	CertificateFormat string
	// IdentityFileOut is an argument to --out flag
	IdentityFileOut string
	// IdentityFormat (used for --format flag for 'tsh login') defines which
	// format to use with --out to store a freshly retrieved certificate
	IdentityFormat identityfile.Format
	// IdentityOverwrite when true will overwrite any existing identity file at
	// IdentityFileOut. When false, user will be prompted before overwriting
	// any files.
	IdentityOverwrite bool

	// BindAddr is an address in the form of host:port to bind to
	// during `tsh login` command
	BindAddr string
	// CallbackAddr is the optional base URL to give to the user when performing
	// SSO redirect flows.
	CallbackAddr string

	// AuthConnector is the name of the connector to use.
	AuthConnector string

	// MFAMode is the preferred mode for MFA/Passwordless assertions.
	MFAMode string

	// SkipVersionCheck skips version checking for client and server
	SkipVersionCheck bool

	// Options is a list of OpenSSH options in the format used in the
	// configuration file.
	Options []string

	// Verbose is used to print extra output.
	Verbose bool

	// Format is used to change the format of output
	Format  string
	OutFile string

	// PlaySpeed controls the playback speed for tsh play.
	PlaySpeed string

	// SearchKeywords is a list of search keywords to match against resource field values.
	SearchKeywords string

	// PredicateExpression defines boolean conditions that will be matched against the resource.
	PredicateExpression string

	// Labels is used to hold labels passed via --labels=k1=v2,k2=v2,,, flag for resource filtering.
	// explicitly passed --labels overrides user@labels positional arg form.
	// NOTE: no command currently supports both, try to keep it that way.
	Labels string

	// NoRemoteExec will not execute a remote command after connecting to a host,
	// will block instead. Useful when port forwarding. Equivalent of -N for OpenSSH.
	NoRemoteExec bool

	// X11ForwardingUntrusted will set up untrusted X11 forwarding for the session ('ssh -X')
	X11ForwardingUntrusted bool

	// X11Forwarding will set up trusted X11 forwarding for the session ('ssh -Y')
	X11ForwardingTrusted bool

	// X11ForwardingTimeout can optionally set to set a timeout for untrusted X11 forwarding.
	X11ForwardingTimeout time.Duration

	// Debug sets log level to debug and sends logs to stdout.
	Debug bool
	// DebugSetByUser specifies whether the flag was set by the user.
	DebugSetByUser bool

	// OSLog sends logs to the unified log system on macOS.
	OSLog bool
	// OSLogSetByUser specifies whether the flag was set by the user or not. This makes it possible to
	// enable OSLog through env var and then disable it selectively with --no-os-log.
	OSLogSetByUser bool

	// Browser can be used to pass the name of a browser to override the system default
	// (not currently implemented), or set to 'none' to suppress browser opening entirely.
	Browser string

	// UseLocalSSHAgent set to false will prevent this client from attempting to
	// connect to the local ssh-agent (or similar) socket at $SSH_AUTH_SOCK.
	//
	// Deprecated in favor of `AddKeysToAgent`.
	UseLocalSSHAgent bool

	// AddKeysToAgent specifies the behavior of how certs are handled.
	AddKeysToAgent string

	// EnableEscapeSequences will scan stdin for SSH escape sequences during
	// command/shell execution. This also requires stdin to be an interactive
	// terminal.
	EnableEscapeSequences bool

	// PreserveAttrs preserves access/modification times from the original file.
	PreserveAttrs bool

	// RequestTTL is the expiration time of the Access Request (how long it
	// will await approval).
	RequestTTL time.Duration

	// SessionTTL is the expiration time for the elevated certificate that will
	// be issued if the Access Request is approved.
	SessionTTL time.Duration

	// MaxDuration specifies how long the access will be granted for.
	MaxDuration time.Duration

	// executablePath is the absolute path to the current executable.
	executablePath string

	// unsetEnvironment unsets Teleport related environment variables.
	unsetEnvironment bool

	// OverrideStdout allows to switch standard output source for resource command. Used in tests.
	OverrideStdout io.Writer
	// overrideStderr allows to switch standard error source for resource command. Used in tests.
	overrideStderr io.Writer
	// overrideStdin allows to switch standard in source for resource command. Used in tests.
	overrideStdin io.Reader

	// MockSSOLogin used in tests to override sso login handler in teleport client.
	MockSSOLogin client.SSOLoginFunc

	// MockHeadlessLogin used in tests to override Headless login handler in teleport client.
	MockHeadlessLogin client.SSHLoginFunc

	// overrideMySQLOptionFilePath overrides the MySQL option file path to use.
	// Useful in parallel tests so they don't all use the default path in the
	// user home dir.
	overrideMySQLOptionFilePath string

	// overridePostgresServiceFilePath overrides the Postgres service file path.
	// Useful in parallel tests so they don't all use the default path in the
	// user home dir.
	overridePostgresServiceFilePath string

	// HomePath is where tsh stores profiles
	HomePath string

	// GlobalTshConfigPath is a path to global TSH config. Can be overridden with TELEPORT_GLOBAL_TSH_CONFIG.
	GlobalTshConfigPath string

	// InsecureListenAnywhere, when set, allows local proxy listener to use any address other than loopback addresses (127/8).
	InsecureListenAnywhere bool
	// LocalProxyAddr is an address used by local proxy listener.
	LocalProxyAddr string
	// LocalProxyPort is a port used by local proxy listener.
	LocalProxyPort string
	// LocalProxyPortMapping is a listening port and an optional target port used by local proxy
	// listener, in the form of "1234" or "1234:5678".
	LocalProxyPortMapping string
	// LocalProxyTunnel specifies whether local proxy will open auth'd tunnel.
	LocalProxyTunnel bool
	// TargetPort is a port used for routing connections to multi-port TCP apps.
	TargetPort uint16

	// Exec is the command to run via tsh aws.
	Exec string
	// AWSRole is Amazon Role ARN or role name that will be used for AWS CLI access.
	AWSRole string
	// AWSCommandArgs contains arguments that will be forwarded to AWS CLI binary.
	AWSCommandArgs []string
	// AWSEndpointURLMode is an AWS proxy mode that serves an AWS endpoint URL
	// proxy instead of an HTTPS proxy.
	// TODO(gabrielcorado): DELETE IN 19.0.0
	AWSEndpointURLMode bool

	// AzureIdentity is Azure identity that will be used for Azure CLI access.
	AzureIdentity string
	// AzureCommandArgs contains arguments that will be forwarded to Azure CLI binary.
	AzureCommandArgs []string

	// GCPServiceAccount is GCP service account name that will be used for GCP CLI access.
	GCPServiceAccount string
	// GCPCommandArgs contains arguments that will be forwarded to GCP CLI binary.
	GCPCommandArgs []string

	// Reason is the reason for starting an ssh or kube session.
	Reason string

	// Invited is a list of invited users to an ssh or kube session.
	Invited []string

	// JoinMode is the participant mode someone is joining a session as.
	JoinMode string

	// SessionKinds is the kind of active sessions to list.
	SessionKinds []string

	// displayParticipantRequirements is set if verbose participant requirement information should be printed for moderated sessions.
	displayParticipantRequirements bool

	// TSHConfig is the loaded tsh configuration file ~/.tsh/config/config.yaml.
	TSHConfig client.TSHConfig

	// ListAll specifies if an ls command should return results from all clusters and proxies.
	ListAll bool

	// SampleTraces indicates whether traces should be sampled.
	SampleTraces bool

	// TraceExporter is a manually provided URI to send traces to instead of
	// forwarding them to the Auth service.
	TraceExporter string

	// TracingProvider is the provider to use to create tracers, from which spans can be created.
	TracingProvider oteltrace.TracerProvider

	// disableAccessRequest disables automatic resource access requests. Deprecated in favor of RequestType.
	disableAccessRequest bool

	// FromUTC is the start time to use for the range of sessions listed by the session recordings listing command
	FromUTC string

	// ToUTC is the start time to use for the range of sessions listed by the session recordings listing command
	ToUTC string

	// maxRecordingsToShow is the maximum number of session recordings to show per page of results
	maxRecordingsToShow int

	// recordingsSince is a duration which sets the time into the past in which to list session recordings
	recordingsSince string

	// command is the selected command (and subcommands) parsed from command
	// line args. Note that this command does not contain the binary (e.g. tsh).
	command string

	// cmdRunner is a custom function to execute provided exec.Cmd. Mainly used
	// in testing.
	cmdRunner func(*exec.Cmd) error
	// kubernetesImpersonationConfig allows to configure custom kubernetes impersonation values.
	kubernetesImpersonationConfig impersonationConfig
	// kubeNamespace allows to configure the default Kubernetes namespace.
	kubeNamespace string

	// kubeAllNamespaces allows users to search for resources in every namespace.
	kubeAllNamespaces bool

	// kubeResourceKind allows to search for resources.
	kubeResourceKind string

	// kubeAPIGroup allows to search for CRD and unknown resources.
	kubeAPIGroup string

	// KubeConfigPath is the location of the Kubeconfig for the current test.
	// Setting this value allows Teleport tests to run `tsh login` commands in
	// parallel.
	// It shouldn't be used outside testing.
	KubeConfigPath string

	// Client only version display.  Skips checking proxy version.
	clientOnlyVersionCheck bool

	// tracer is the tracer used to trace tsh commands.
	tracer oteltrace.Tracer

	// Headless uses headless login for the client session.
	Headless bool

	// MlockMode determines whether the process memory will be locked, and whether errors will be enforced.
	// Allowed values include false, strict, and best_effort.
	MlockMode string

	// HeadlessAuthenticationID is the ID of a headless authentication.
	HeadlessAuthenticationID string

	// headlessSkipConfirm determines whether to provide a y/N
	// confirmation prompt before prompting for MFA.
	headlessSkipConfirm bool

	// DTAuthnRunCeremony allows tests to override the default device
	// authentication function.
	// Defaults to [dtauthn.NewCeremony().Run].
	DTAuthnRunCeremony client.DTAuthnRunCeremonyFunc

	// DTAutoEnroll allows tests to override the default device
	// auto-enroll function.
	// Defaults to [dtenroll.AutoEnroll].
	DTAutoEnroll client.DTAutoEnrollFunc

	// WebauthnLogin allows tests to override the Webauthn Login func.
	// Defaults to [wancli.Login].
	WebauthnLogin client.WebauthnLoginFunc

	// LeafClusterName is the optional name of a leaf cluster to connect to instead
	LeafClusterName string

	// PIVSlot specifies a specific PIV slot to use with hardware key support.
	PIVSlot string

	// SSHLogDir is the directory to log the output of multiple SSH commands to.
	// If not set, no logs will be created.
	SSHLogDir string

	// DisableSSHResumption disables transparent SSH connection resumption.
	DisableSSHResumption bool

	// Relogin determines if a login attempt should be made in the event of command failures. This
	// allows users with potentially stale credentials preventing access to gain the required access
	// without having to manually run tsh login and the failed command again.
	Relogin bool

	// profileStatusOverride overrides return of ProfileStatus(). used in tests.
	profileStatusOverride *client.ProfileStatus

	// lookPathOverride overrides return of LookPath(). used in tests.
	lookPathOverride string

	// HardwareKeyAgentServer determines whether `tsh daemon` will run the hardware key agent server.
	HardwareKeyAgentServer bool
	// disableHardwareKeyAgentClient determines whether the client will attempt to connect
	// to the hardware key agent. Some commands, like login, are better with the
	// direct PIV service so that prompts are not split between processes.
	disableHardwareKeyAgentClient bool

	// ParallelJobs specifies the number of parallel jobs allowed.
	ParallelJobs int
	// OutputDir specifies the directory for storing command outputs.
	OutputDir string
	// Confirm determines whether to provide a y/N confirmation prompt.
	Confirm bool

	// clientStore is the client identity storage interface. This store must be initialized once
	// and only once in order to ensure key (and hardware key) storage is synced across the process.
	//
	// Use getClientStore instead of using this directly to ensure the client store is initialized,
	// instead of performing nil checks.
	clientStore *client.Store
	// clientStoreSet ensures that the client store is only initialized once. Generally, using an
	// atomic here is overkill as the CLIConf is generally consumed sequentially. However, occasionally
	// we need concurrency safety, such as for [forEachProfileParallel].
	clientStoreSet int32

	// databaseMCPRegistryOverride overrides database access MCP servers
	// registry. used in tests.
	databaseMCPRegistryOverride dbmcp.Registry

	kingpinApp *kingpin.Application
	// ForkAfterAuthentication indicates that tsh should go into the background
	// after authentication.
	ForkAfterAuthentication bool
	// forkSignalFd is the file descriptor for the child process to signal the
	// parent when re-execing.
	forkSignalFd uint64
	// forkKillFd is the file descriptor for the child process to check the
	// parent's state when re-execing.
	forkKillFd uint64

	// checkManagedUpdates initiates check of managed update after client connects to cluster.
	checkManagedUpdates bool
}

func (c *CLIConf) isForkAuthChild() bool {
	return isValidForkSignalFd(c.forkSignalFd) && isValidForkSignalFd(c.forkKillFd)
}

// Stdout returns the stdout writer.
func (c *CLIConf) Stdout() io.Writer {
	if c.OverrideStdout != nil {
		return c.OverrideStdout
	}
	return os.Stdout
}

// Stderr returns the stderr writer.
func (c *CLIConf) Stderr() io.Writer {
	if c.overrideStderr != nil {
		return c.overrideStderr
	}
	return os.Stderr
}

// Stdin returns the stdin reader.
func (c *CLIConf) Stdin() io.Reader {
	if c.overrideStdin != nil {
		return c.overrideStdin
	}
	return os.Stdin
}

// CommandWithBinary returns the current/selected command with the binary.
func (c *CLIConf) CommandWithBinary() string {
	return fmt.Sprintf("%s %s", teleport.ComponentTSH, c.command)
}

// RunCommand executes provided command.
func (c *CLIConf) RunCommand(cmd *exec.Cmd) error {
	if c.cmdRunner != nil {
		return trace.Wrap(c.cmdRunner(cmd))
	}
	return trace.Wrap(cmd.Run())
}

// LookPath searches for an executable named file.
func (c *CLIConf) LookPath(file string) (string, error) {
	if c.lookPathOverride != "" {
		return c.lookPathOverride, nil
	}
	return exec.LookPath(file)
}

// PromptConfirmation prompts the user for a yes/no confirmation for question.
// The prompt is skipped unless cf.Confirm is set.
func (c *CLIConf) PromptConfirmation(question string) error {
	if !c.Confirm {
		fmt.Fprintf(c.Stdout(), "Skipping confirmation for %q due to the --no-confirm flag.\n", question)
		return nil
	}

	ok, err := prompt.Confirmation(c.Context, c.Stdout(), prompt.Stdin(), question)
	if err != nil {
		return trace.Wrap(err)
	} else if !ok {
		return trace.Errorf("Operation canceled by user request.")
	}
	return nil
}

func Main() {
	cmdLineOrig := os.Args[1:]
	var cmdLine []string

	ctx, cancel := stacksignal.GetSignalHandler().NotifyContext(context.Background())
	defer cancel()

	// lets see: if the executable name is 'ssh' or 'scp' we convert
	// that to "tsh ssh" or "tsh scp"
	switch filepath.Base(os.Args[0]) {
	case "ssh":
		cmdLine = append([]string{"ssh"}, cmdLineOrig...)
	case "scp":
		cmdLine = append([]string{"scp"}, cmdLineOrig...)
	default:
		cmdLine = cmdLineOrig
	}

	err := Run(ctx, cmdLine)
	prompt.NotifyExit() // Allow prompt to restore terminal state on exit.
	if err != nil {
		var exitError *common.ExitCodeError
		if errors.As(err, &exitError) {
			os.Exit(exitError.Code)
		}
		utils.FatalError(err)
	}
}

const (
	authEnvVar                = "TELEPORT_AUTH"
	clusterEnvVar             = "TELEPORT_CLUSTER"
	kubeClusterEnvVar         = "TELEPORT_KUBE_CLUSTER"
	loginEnvVar               = "TELEPORT_LOGIN"
	bindAddrEnvVar            = "TELEPORT_LOGIN_BIND_ADDR"
	browserEnvVar             = "TELEPORT_LOGIN_BROWSER"
	proxyEnvVar               = "TELEPORT_PROXY"
	headlessEnvVar            = "TELEPORT_HEADLESS"
	headlessSkipConfirmEnvVar = "TELEPORT_HEADLESS_SKIP_CONFIRM"
	// TELEPORT_SITE uses the older deprecated "site" terminology to refer to a
	// cluster. All new code should use TELEPORT_CLUSTER instead.
	siteEnvVar                = "TELEPORT_SITE"
	userEnvVar                = "TELEPORT_USER"
	addKeysToAgentEnvVar      = "TELEPORT_ADD_KEYS_TO_AGENT"
	useLocalSSHAgentEnvVar    = "TELEPORT_USE_LOCAL_SSH_AGENT"
	globalTshConfigEnvVar     = "TELEPORT_GLOBAL_TSH_CONFIG"
	mfaModeEnvVar             = "TELEPORT_MFA_MODE"
	mlockModeEnvVar           = "TELEPORT_MLOCK_MODE"
	identityFileEnvVar        = "TELEPORT_IDENTITY_FILE"
	gcloudSecretEnvVar        = "TELEPORT_GCLOUD_SECRET"
	awsAccessKeyIDEnvVar      = "TELEPORT_AWS_ACCESS_KEY_ID"
	awsSecretAccessKeyEnvVar  = "TELEPORT_AWS_SECRET_ACCESS_KEY"
	awsRegionEnvVar           = "TELEPORT_AWS_REGION"
	awsKeystoreEnvVar         = "TELEPORT_AWS_KEYSTORE"
	awsWorkgroupEnvVar        = "TELEPORT_AWS_WORKGROUP"
	proxyKubeConfigEnvVar     = "TELEPORT_KUBECONFIG"
	noResumeEnvVar            = "TELEPORT_NO_RESUME"
	requestModeEnvVar         = "TELEPORT_REQUEST_MODE"
	mcpClientConfigEnvVar     = "TELEPORT_MCP_CLIENT_CONFIG"
	mcpConfigJSONFormatEnvVar = "TELEPORT_MCP_CONFIG_JSON_FORMAT"
	toolsCheckUpdateEnvVar    = "TELEPORT_TOOLS_CHECK_UPDATE"

	clusterHelp = "Specify the Teleport cluster to connect."
	browserHelp = "Set to 'none' to suppress browser opening on login."
	searchHelp  = `List of comma separated search keywords or phrases enclosed in quotations (e.g. --search=foo,bar,"some phrase").`
	queryHelp   = `Query by predicate language enclosed in single quotes. Supports ==, !=, &&, and || (e.g. --query='labels["key1"] == "value1" && labels["key2"] != "value2"').`
	labelHelp   = "List of comma separated labels to filter by labels (e.g. key1=value1,key2=value2)."
	quietHelp   = "Quiet mode."
	// proxyDefaultResolutionTimeout is how long to wait for an unknown proxy
	// port to be resolved.
	//
	// Originally based on the RFC-8305 "Maximum Connection Attempt Delay"
	// recommended default value of 2s. In the RFC this value is for the
	// establishment of a TCP connection, rather than the full HTTP round-
	// trip that we measure against, so some tweaking may be needed.
	//
	// Raised to 5 seconds when fallback measure was removed to account for
	// users with higher latency connections.
	proxyDefaultResolutionTimeout = 5 * time.Second
)

// env vars that tsh status will check to provide hints about active env vars to a user.
var tshStatusEnvVars = [...]string{proxyEnvVar, clusterEnvVar, siteEnvVar, kubeClusterEnvVar, teleport.EnvKubeConfig}

// CliOption is used in tests to inject/override configuration within Run
type CliOption func(*CLIConf) error

// Run executes TSH client. same as main() but easier to test. Note that this
// function modifies global state in `tsh` (e.g. the system logger), and WILL
// ALSO MODIFY EXTERNAL SHARED STATE in its default configuration (e.g. the
// $HOME/.tsh dir, $KUBECONFIG, etc).
//
// DO NOT RUN TESTS that call Run() in parallel (unless you taken precautions).
func Run(ctx context.Context, args []string, opts ...CliOption) error {
	cf := CLIConf{
		Context:            ctx,
		TracingProvider:    tracing.NoopProvider(),
		DTAuthnRunCeremony: dtauthn.NewCeremony().Run,
		DTAutoEnroll:       dtenroll.AutoEnroll,
	}

	// We need to parse the arguments before executing managed updates to identify
	// the profile name and the required version for the current cluster.
	// All other commands and flags may change between versions, so full parsing
	// should be performed only after managed updates are applied.
	var proxyArg string
	muApp := utils.InitHiddenCLIParser()
	muApp.Flag("proxy", "Teleport proxy address").Envar(proxyEnvVar).Hidden().StringVar(&proxyArg)
	muApp.Flag("check-update", "Check for availability of managed update.").Envar(toolsCheckUpdateEnvVar).Hidden().BoolVar(&cf.checkManagedUpdates)
	if _, err := muApp.Parse(utils.FilterArguments(args, muApp.Model())); err != nil {
		slog.WarnContext(ctx, "can't identify current profile", "error", err)
	}
	// Check local update for specific proxy from configuration.
	name := utils.TryHost(strings.TrimPrefix(strings.ToLower(proxyArg), "https://"))
	if err := tools.CheckAndUpdateLocal(ctx, name, args); err != nil {
		return trace.Wrap(err)
	}

	// run early to enable debug logging if env var is set.
	// this makes it possible to debug early startup functionality, particularly command aliases.
	if _, err := initLogger(&cf, utils.LoggingForCLI, parseLoggingOptsFromEnv()); err != nil {
		printInitLoggerError(err)
	}

	moduleCfg := modules.GetModules()
	var cpuProfile, memProfile, traceProfile string

	// configure CLI argument parser:
	cf.kingpinApp = utils.InitCLIParser("tsh", "Teleport Command Line Client.").Interspersed(true)
	app := cf.kingpinApp

	app.Flag("login", "Remote host login.").Short('l').Envar(loginEnvVar).StringVar(&cf.NodeLogin)
	app.Flag("proxy", "Teleport proxy address.").Envar(proxyEnvVar).StringVar(&cf.Proxy)
	app.Flag("nocache", "Do not cache cluster discovery locally.").Hidden().BoolVar(&cf.NoCache)
	app.Flag("user", "Teleport user, defaults to current local user.").Envar(userEnvVar).StringVar(&cf.Username)
	app.Flag("mem-profile", "Write memory profile to file.").Hidden().StringVar(&memProfile)
	app.Flag("cpu-profile", "Write CPU profile to file.").Hidden().StringVar(&cpuProfile)
	app.Flag("trace-profile", "Write trace profile to file.").Hidden().StringVar(&traceProfile)
	app.Flag("option", "").Short('o').Hidden().AllowDuplicate().PreAction(func(ctx *kingpin.ParseContext) error {
		return trace.BadParameter("invalid flag, perhaps you want to use this flag as tsh ssh -o?")
	}).String()

	app.Flag("ttl", "Minutes to live for a session.").Int32Var(&cf.MinsToLive)
	app.Flag("identity", "Identity file.").Short('i').Envar(identityFileEnvVar).StringVar(&cf.IdentityFileIn)
	app.Flag("compat", "OpenSSH compatibility flag.").Hidden().StringVar(&cf.Compatibility)
	app.Flag("cert-format", "SSH certificate format.").StringVar(&cf.CertificateFormat)
	app.Flag("trace", "Capture and export distributed traces.").Hidden().BoolVar(&cf.SampleTraces)
	app.Flag("trace-exporter", "An OTLP exporter URL to send spans to. Note - only tsh spans will be included.").Hidden().StringVar(&cf.TraceExporter)
	// This flag only applies to tsh ssh; it's defined here to make configuring
	// the re-exec command easier.
	app.Flag("fork-signal-fd", "File descriptor to signal parent on when forked. Overrides --fork-after-authentication. For internal use only.").Hidden().Uint64Var(&cf.forkSignalFd)
	app.Flag("fork-kill-fd", "File descriptor to check parent health on when forked. For internal use only.").Hidden().Uint64Var(&cf.forkKillFd)

	if !moduleCfg.IsBoringBinary() {
		// The user is *never* allowed to do this in FIPS mode.
		app.Flag("insecure", "Do not verify server's certificate and host name. Use only in test environments.").
			Default("false").
			BoolVar(&cf.InsecureSkipVerify)
	}

	app.Flag("auth", "Specify the name of authentication connector to use.").Envar(authEnvVar).StringVar(&cf.AuthConnector)
	app.Flag("skip-version-check", "Skip version checking between server and client.").BoolVar(&cf.SkipVersionCheck)
	// we don't want to add `.Envar(debugEnvVar)` here:
	// - we already process TELEPORT_DEBUG with initLogger(), so we don't need to do it second time
	// - Kingpin is strict about syntax, so TELEPORT_DEBUG=rubbish will crash a program; we don't want such behavior for this variable.
	app.Flag("debug", "Verbose logging to stdout.").Short('d').IsSetByUser(&cf.DebugSetByUser).BoolVar(&cf.Debug)
	osLogFlag := app.Flag("os-log",
		fmt.Sprintf("Verbose logging to the unified logging system. This flag implies --debug. Also available through the %s env var. More details see https://goteleport.com/docs/connect-your-client/tsh/#debug-logs.",
			osLogEnvVar)).
		IsSetByUser(&cf.OSLogSetByUser)
	if runtime.GOOS != constants.DarwinOS {
		osLogFlag.Hidden()
	}
	osLogFlag.BoolVar(&cf.OSLog)
	app.Flag("add-keys-to-agent", fmt.Sprintf("Controls how keys are handled. Valid values are %v.", client.AllAddKeysOptions)).Short('k').Envar(addKeysToAgentEnvVar).Default(client.AddKeysToAgentAuto).StringVar(&cf.AddKeysToAgent)
	app.Flag("use-local-ssh-agent", "Deprecated in favor of the add-keys-to-agent flag.").
		Hidden().
		Envar(useLocalSSHAgentEnvVar).
		Default("true").
		BoolVar(&cf.UseLocalSSHAgent)
	app.Flag("enable-escape-sequences", "Enable support for SSH escape sequences. Type '~?' during an SSH session to list supported sequences. Default is enabled.").
		Default("true").
		BoolVar(&cf.EnableEscapeSequences)
	app.Flag("bind-addr", "Override host:port used when opening a browser for cluster logins.").Envar(bindAddrEnvVar).StringVar(&cf.BindAddr)
	app.Flag("callback", "Override the base URL (host:port) of the link shown when opening a browser for cluster logins. Must be used with --bind-addr.").StringVar(&cf.CallbackAddr)
	app.Flag("browser-login", browserHelp).Hidden().Envar(browserEnvVar).StringVar(&cf.Browser)
	modes := []string{mfaModeAuto, mfaModeCrossPlatform, mfaModePlatform, mfaModeOTP, mfaModeSSO}
	app.Flag("mfa-mode", fmt.Sprintf("Preferred mode for MFA and Passwordless assertions (%v).", strings.Join(modes, ", "))).
		Default(mfaModeAuto).
		Envar(mfaModeEnvVar).
		EnumVar(&cf.MFAMode, modes...)
	app.Flag("headless", "Use headless login. Shorthand for --auth=headless.").Envar(headlessEnvVar).BoolVar(&cf.Headless)
	app.Flag("mlock", fmt.Sprintf("Determines whether process memory will be locked and whether failure to do so will be accepted (%v).", strings.Join(mlockModes, ", "))).
		Default(mlockModeAuto).
		Envar(mlockModeEnvVar).
		StringVar(&cf.MlockMode)
	app.HelpFlag.Short('h')
	app.Flag("piv-slot", "Specify a PIV slot key to use for Hardware Key support instead of the default. Ex: \"9d\".").Envar("TELEPORT_PIV_SLOT").StringVar(&cf.PIVSlot)
	app.Flag("check-update", "Check for availability of managed update.").Envar(toolsCheckUpdateEnvVar).Hidden().BoolVar(&cf.checkManagedUpdates)

	ver := app.Command("version", "Print the tsh client and Proxy server versions for the current context.")
	ver.Flag("format", defaults.FormatFlagDescription(defaults.DefaultFormats...)).Short('f').Default(teleport.Text).EnumVar(&cf.Format, defaults.DefaultFormats...)
	ver.Flag("client", "Show the client version only (no server required).").
		BoolVar(&cf.clientOnlyVersionCheck)
	// ssh
	// Use Interspersed(false) to forward all flags to ssh.
	ssh := app.Command("ssh", "Run shell or execute a command on a remote SSH node.").Interspersed(false)
	ssh.Arg("[user@]host", "Remote hostname and the login to use, this argument is required.").StringVar(&cf.UserHost)
	ssh.Arg("command", "Command to execute on a remote host.").StringsVar(&cf.RemoteCommand)
	app.Flag("jumphost", "SSH jumphost.").Short('J').StringVar(&cf.ProxyJump)
	ssh.Flag("port", "SSH port on a remote host.").Short('p').Int32Var(&cf.NodePort)
	ssh.Flag("forward-agent", "Forward agent to target node.").Short('A').BoolVar(&cf.ForwardAgent)
	ssh.Flag("forward", "Forward localhost connections to remote server.").Short('L').StringsVar(&cf.LocalForwardPorts)
	ssh.Flag("dynamic-forward", "Forward localhost connections to remote server using SOCKS5.").Short('D').StringsVar(&cf.DynamicForwardedPorts)
	ssh.Flag("remote-forward", "Forward remote connections to localhost.").Short('R').StringsVar(&cf.RemoteForwardPorts)
	ssh.Flag("local", "Execute command on localhost after connecting to SSH node.").Default("false").BoolVar(&cf.LocalExec)
	ssh.Flag("tty", "Allocate TTY.").Short('t').BoolVar(&cf.Interactive)
	ssh.Flag("cluster", clusterHelp).Short('c').StringVar(&cf.SiteName)
	ssh.Flag("option", "OpenSSH options in the format used in the configuration file.").Short('o').AllowDuplicate().StringsVar(&cf.Options)
	ssh.Flag("no-remote-exec", "Don't execute remote command, useful for port forwarding.").Short('N').BoolVar(&cf.NoRemoteExec)
	ssh.Flag("x11-untrusted", "Requests untrusted (secure) X11 forwarding for this session.").Short('X').BoolVar(&cf.X11ForwardingUntrusted)
	ssh.Flag("x11-trusted", "Requests trusted (insecure) X11 forwarding for this session. This can make your local machine vulnerable to attacks, use with caution.").Short('Y').BoolVar(&cf.X11ForwardingTrusted)
	ssh.Flag("x11-untrusted-timeout", "Sets a timeout for untrusted X11 forwarding, after which the client will reject any forwarding requests from the server.").Default("10m").DurationVar((&cf.X11ForwardingTimeout))
	ssh.Flag("invite", "A comma separated list of people to mark as invited for the session.").StringsVar(&cf.Invited)
	ssh.Flag("reason", "The purpose of the session.").StringVar(&cf.Reason)
	ssh.Flag("participant-req", "Displays a verbose list of required participants in a moderated session.").BoolVar(&cf.displayParticipantRequirements)
	ssh.Flag("request-reason", "Reason for requesting access.").StringVar(&cf.RequestReason)
	ssh.Flag("request-mode", fmt.Sprintf("Type of automatic access request to make (%s).", strings.Join(accessRequestModes, ", "))).Envar(requestModeEnvVar).Default(accessRequestModeResource).EnumVar(&cf.RequestMode, accessRequestModes...)
	ssh.Flag("disable-access-request", "Disable automatic resource access requests (DEPRECATED: use --request-mode=off).").BoolVar(&cf.disableAccessRequest)
	ssh.Flag("log-dir", "Directory to log separated command output, when executing on multiple nodes. If set, output from each node will also be labeled in the terminal.").StringVar(&cf.SSHLogDir)
	ssh.Flag("no-resume", "Disable SSH connection resumption.").Envar(noResumeEnvVar).BoolVar(&cf.DisableSSHResumption)
	ssh.Flag("relogin", "Permit performing an authentication attempt on a failed command.").Default("true").BoolVar(&cf.Relogin)
	ssh.Flag("fork-after-authentication", "Run in background after authentication is complete.").Short('f').BoolVar(&cf.ForkAfterAuthentication)
	// The following flags are OpenSSH compatibility flags. They are used for
	// users that alias "ssh" to "tsh ssh." The following OpenSSH flags are
	// implemented. From "man 1 ssh":
	//
	// * "-V Display the version number and exit."
	// * "-T Disable pseudo-terminal allocation."
	ssh.Flag(uuid.New().String(), "").Short('T').Hidden().BoolVar(&cf.NonInteractive)
	ssh.Flag(uuid.New().String(), "").Short('V').Hidden().BoolVar(&cf.ShowVersion)

	resolve := app.Command("resolve", "Resolves an SSH host.")
	resolve.Arg("host", "Remote hostname to resolve.").Required().StringVar(&cf.UserHost)
	resolve.Flag("quiet", quietHelp).Short('q').BoolVar(&cf.Quiet)
	resolve.Flag("format", defaults.FormatFlagDescription(defaults.DefaultFormats...)).Short('f').Default(teleport.Text).EnumVar(&cf.Format, defaults.DefaultFormats...)

	// Daemon service for teleterm client
	daemon := app.Command("daemon", "Daemon is the tsh daemon service.").Hidden()
	daemonStart := daemon.Command("start", "Starts tsh daemon service.").Hidden()
	daemonStart.Flag("addr", "Addr is the daemon listening address.").StringVar(&cf.DaemonAddr)
	daemonStart.Flag("certs-dir", "Directory containing certs used to create secure gRPC connection with daemon service.").StringVar(&cf.DaemonCertsDir)
	daemonStart.Flag("prehog-addr", "URL where prehog events should be submitted.").StringVar(&cf.DaemonPrehogAddr)
	daemonStart.Flag("kubeconfigs-dir", "Directory containing kubeconfig for Kubernetes Access.").StringVar(&cf.DaemonKubeconfigsDir)
	daemonStart.Flag("agents-dir", "Directory containing agent config files and data directories for Connect My Computer.").StringVar(&cf.DaemonAgentsDir)
	daemonStart.Flag("installation-id", "Unique ID identifying a specific Teleport Connect installation.").StringVar(&cf.DaemonInstallationID)
	daemonStart.Flag("hardware-key-agent", "Serve the hardware key agent as part of the daemon process.").BoolVar(&cf.HardwareKeyAgentServer)
	daemonStop := daemon.Command("stop", "Gracefully stops a process on Windows by sending Ctrl-Break to it.").Hidden()
	daemonStop.Flag("pid", "PID to be stopped.").IntVar(&cf.DaemonPid)

	// AWS.
	// Use Interspersed(false) to forward all flags to AWS CLI.
	aws := app.Command("aws", "Access AWS API.").Interspersed(false)
	aws.Arg("command", "AWS command and subcommands arguments that are going to be forwarded to AWS CLI.").StringsVar(&cf.AWSCommandArgs)
	aws.Flag("app", "Optional Name of the AWS application to use if logged into multiple.").StringVar(&cf.AppName)
	aws.Flag("endpoint-url", "Run local proxy to serve as an AWS endpoint URL. If not specified, local proxy serves as an HTTPS proxy.").
		Short('e').Hidden().BoolVar(&cf.AWSEndpointURLMode)
	aws.Flag("exec", "Execute different commands (e.g. terraform) under Teleport credentials.").StringVar(&cf.Exec)
	aws.Flag("aws-role", "(For AWS CLI access only) Amazon IAM role ARN or role name.").StringVar(&cf.AWSRole)

	azure := app.Command("az", "Access Azure API.").Interspersed(false)
	azure.Arg("command", "`az` command and subcommands arguments that are going to be forwarded to Azure CLI.").StringsVar(&cf.AzureCommandArgs)
	azure.Flag("app", "Optional name of the Azure application to use if logged into multiple.").StringVar(&cf.AppName)
	azure.Flag("azure-identity", "(For Azure CLI access only) Azure managed identity name.").StringVar(&cf.AzureIdentity)

	gcloud := app.Command("gcloud", "Access GCP API with the gcloud command.").Interspersed(false)
	gcloud.Arg("command", "`gcloud` command and subcommands arguments.").StringsVar(&cf.GCPCommandArgs)
	gcloud.Flag("app", "Optional name of the GCP application to use if logged into multiple.").StringVar(&cf.AppName)
	gcloud.Flag("gcp-service-account", "(For GCP CLI access only) GCP service account name.").StringVar(&cf.GCPServiceAccount)
	gcloud.Alias("gcp")

	gsutil := app.Command("gsutil", "Access Google Cloud Storage with the gsutil command.").Interspersed(false)
	gsutil.Arg("command", "`gsutil` command and subcommands arguments.").StringsVar(&cf.GCPCommandArgs)
	gsutil.Flag("app", "Optional name of the GCP application to use if logged into multiple.").StringVar(&cf.AppName)
	gsutil.Flag("gcp-service-account", "(For GCP CLI access only) GCP service account name.").StringVar(&cf.GCPServiceAccount)

	// Applications.
	apps := app.Command("apps", "View and control proxied applications.").Alias("app")
	apps.Flag("cluster", clusterHelp).Short('c').StringVar(&cf.SiteName)
	lsApps := apps.Command("ls", "List available applications.")
	lsApps.Flag("verbose", "Show extra application fields.").Short('v').BoolVar(&cf.Verbose)
	lsApps.Flag("search", searchHelp).StringVar(&cf.SearchKeywords)
	lsApps.Flag("query", queryHelp).StringVar(&cf.PredicateExpression)
	lsApps.Flag("format", defaults.FormatFlagDescription(defaults.DefaultFormats...)).Short('f').Default(teleport.Text).EnumVar(&cf.Format, defaults.DefaultFormats...)
	lsApps.Arg("labels", labelHelp).StringVar(&cf.Labels)
	lsApps.Flag("all", "List apps from all clusters and proxies.").Short('R').BoolVar(&cf.ListAll)
	appLogin := apps.Command("login", "Retrieve short-lived certificate for an app.")
	appLogin.Arg("app", "App name to retrieve credentials for. Can be obtained from `tsh apps ls` output.").Required().StringVar(&cf.AppName)
	appLogin.Flag("aws-role", "(For AWS CLI access only) Amazon IAM role ARN or role name.").StringVar(&cf.AWSRole)
	appLogin.Flag("azure-identity", "(For Azure CLI access only) Azure managed identity name.").StringVar(&cf.AzureIdentity)
	appLogin.Flag("gcp-service-account", "(For GCP CLI access only) GCP service account name.").StringVar(&cf.GCPServiceAccount)
	appLogin.Flag("target-port", "Port to which connections made using this cert should be routed to. Valid only for multi-port TCP apps.").Uint16Var(&cf.TargetPort)
	appLogin.Flag("quiet", quietHelp).Short('q').BoolVar(&cf.Quiet)
	appLogout := apps.Command("logout", "Remove app certificate.")
	appLogout.Arg("app", "App to remove credentials for.").StringVar(&cf.AppName)
	appConfig := apps.Command("config", "Print app connection information.")
	appConfig.Arg("app", "App to print information for. Required when logged into multiple apps.").StringVar(&cf.AppName)
	appConfig.Flag("format", fmt.Sprintf("Optional print format, one of: %q to print app address, %q to print CA cert path, %q to print cert path, %q print key path, %q to print example curl command, %q or %q to print everything as JSON or YAML.",
		appFormatURI, appFormatCA, appFormatCert, appFormatKey, appFormatCURL, appFormatJSON, appFormatYAML),
	).Short('f').StringVar(&cf.Format)

	// Recordings.
	recordings := app.Command("recordings", "View and control session recordings.").Alias("recording")
	lsRecordings := recordings.Command("ls", "List recorded sessions.")
	lsRecordings.Flag("format", defaults.FormatFlagDescription(defaults.DefaultFormats...)+". Defaults to 'text'.").Short('f').Default(teleport.Text).EnumVar(&cf.Format, defaults.DefaultFormats...)
	lsRecordings.Flag("from-utc", fmt.Sprintf("Start of time range in which recordings are listed. Format %s. Defaults to 24 hours ago.", defaults.TshTctlSessionListTimeFormat)).StringVar(&cf.FromUTC)
	lsRecordings.Flag("to-utc", fmt.Sprintf("End of time range in which recordings are listed. Format %s. Defaults to current time.", defaults.TshTctlSessionListTimeFormat)).StringVar(&cf.ToUTC)
	lsRecordings.Flag("limit", fmt.Sprintf("Maximum number of recordings to show. Default %s.", defaults.TshTctlSessionListLimit)).Default(defaults.TshTctlSessionListLimit).IntVar(&cf.maxRecordingsToShow)
	lsRecordings.Flag("last", "Duration into the past from which session recordings should be listed. Format \"5h30m40s\".").StringVar(&cf.recordingsSince)
	exportRecordings := recordings.Command("export", "Export recorded desktop sessions to video.")
	exportRecordings.Flag("out", "Override output file name.").StringVar(&cf.OutFile)
	exportRecordings.Arg("session-id", "ID of the session to export.").Required().StringVar(&cf.SessionID)

	// Local TLS proxy.
	proxy := app.Command("proxy", "Run local TLS proxy allowing connecting to Teleport in single-port mode.")
	proxySSH := proxy.Command("ssh", "Start local TLS proxy for ssh connections when using Teleport in single-port mode.")
	proxySSH.Arg("[user@]host", "Remote hostname and the login to use.").Required().StringVar(&cf.UserHost)
	proxySSH.Flag("cluster", clusterHelp).Short('c').StringVar(&cf.SiteName)
	proxySSH.Flag("no-resume", "Disable SSH connection resumption.").Envar(noResumeEnvVar).BoolVar(&cf.DisableSSHResumption)
	proxySSH.Flag("relogin", "Permit performing an authentication attempt on a failed command.").Default("true").BoolVar(&cf.Relogin)
	proxyDB := proxy.Command("db", "Start local TLS proxy for database connections when using Teleport in single-port mode.")
	// don't require <db> positional argument, user can select with --labels/--query alone.
	proxyDB.Arg("db", "The name of the database to start local proxy for.").StringVar(&cf.DatabaseService)
	proxyDB.Flag("insecure-listen-anywhere", "Allows the local proxy to listen on any address without restrictions. WARNING: this will expose unsecured listener to anyone in the network. Only use when network access is otherwise restricted.").BoolVar(&cf.InsecureListenAnywhere)
	proxyDB.Flag("listen", "Specifies the source address used by proxy db listener. Mutually exclusive with --port.").StringVar(&cf.LocalProxyAddr)
	proxyDB.Flag("port", "Specifies the source port used by proxy db listener.").Short('p').StringVar(&cf.LocalProxyPort)
	proxyDB.Flag("tunnel", "Open authenticated tunnel using database's client certificate so clients don't need to authenticate.").BoolVar(&cf.LocalProxyTunnel)
	proxyDB.Flag("db-user", "Database user to log in as.").Short('u').StringVar(&cf.DatabaseUser)
	proxyDB.Flag("db-name", "Database name to log in to.").Short('n').StringVar(&cf.DatabaseName)
	proxyDB.Flag("db-roles", "List of comma separate database roles to use for auto-provisioned user.").Short('r').StringVar(&cf.DatabaseRoles)
	proxyDB.Flag("cluster", clusterHelp).Short('c').StringVar(&cf.SiteName)
	proxyDB.Flag("labels", labelHelp).StringVar(&cf.Labels)
	proxyDB.Flag("query", queryHelp).StringVar(&cf.PredicateExpression)
	proxyDB.Flag("request-reason", "Reason for requesting access.").StringVar(&cf.RequestReason)
	proxyDB.Flag("disable-access-request", "Disable automatic resource access requests.").BoolVar(&cf.disableAccessRequest)

	proxyApp := proxy.Command("app", "Start local TLS proxy for app connection when using Teleport in single-port mode.")
	proxyApp.Arg("app", "The name of the application to start local proxy for.").Required().StringVar(&cf.AppName)
	proxyApp.Flag("port", "Specifies the listening port used by by the proxy app listener. Accepts an optional target port of a multi-port TCP app after a colon, e.g. \"1234:5678\".").Short('p').StringVar(&cf.LocalProxyPortMapping)
	proxyApp.Flag("cluster", clusterHelp).Short('c').StringVar(&cf.SiteName)

	proxyAWS := proxy.Command("aws", "Start local proxy for AWS access.")
	proxyAWS.Flag("app", "Optional Name of the AWS application to use if logged into multiple.").StringVar(&cf.AppName)
	proxyAWS.Flag("port", "Specifies the source port used by the proxy listener.").Short('p').StringVar(&cf.LocalProxyPort)
	proxyAWS.Flag("endpoint-url", "Run local proxy to serve as an AWS endpoint URL. If not specified, local proxy serves as an HTTPS proxy.").Short('e').Hidden().BoolVar(&cf.AWSEndpointURLMode)
	proxyAWS.Flag("format", awsProxyFormatFlagDescription()).Short('f').Default(envVarDefaultFormat()).EnumVar(&cf.Format, awsProxyFormats...)

	proxyAzure := proxy.Command("azure", "Start local proxy for Azure access.")
	proxyAzure.Flag("app", "Optional Name of the Azure application to use if logged into multiple.").StringVar(&cf.AppName)
	proxyAzure.Flag("port", "Specifies the source port used by the proxy listener.").Short('p').StringVar(&cf.LocalProxyPort)
	proxyAzure.Flag("format", envVarFormatFlagDescription()).Short('f').Default(envVarDefaultFormat()).EnumVar(&cf.Format, envVarFormats...)
	proxyAzure.Alias("az")

	proxyGcloud := proxy.Command("gcloud", "Start local proxy for GCP access.")
	proxyGcloud.Flag("app", "Optional Name of the GCP application to use if logged into multiple.").StringVar(&cf.AppName)
	proxyGcloud.Flag("port", "Specifies the source port used by the proxy listener.").Short('p').StringVar(&cf.LocalProxyPort)
	proxyGcloud.Flag("format", envVarFormatFlagDescription()).Short('f').Default(envVarDefaultFormat()).EnumVar(&cf.Format, envVarFormats...)
	proxyGcloud.Alias("gcp")

	proxyKube := newProxyKubeCommand(proxy)

	// Databases.
	db := app.Command("db", "View and control proxied databases.")
	db.Flag("cluster", clusterHelp).Short('c').StringVar(&cf.SiteName)
	dbList := db.Command("ls", "List all available databases.")
	dbList.Flag("verbose", "Show extra database fields.").Short('v').BoolVar(&cf.Verbose)
	dbList.Flag("search", searchHelp).StringVar(&cf.SearchKeywords)
	dbList.Flag("query", queryHelp).StringVar(&cf.PredicateExpression)
	dbList.Flag("format", defaults.FormatFlagDescription(defaults.DefaultFormats...)).Short('f').Default(teleport.Text).EnumVar(&cf.Format, defaults.DefaultFormats...)
	dbList.Flag("all", "List databases from all clusters and proxies.").Short('R').BoolVar(&cf.ListAll)
	dbList.Arg("labels", labelHelp).StringVar(&cf.Labels)
	dbList.Alias(dbListHelp)
	dbLogin := db.Command("login", "Retrieve credentials for a database.")
	// don't require <db> positional argument, user can select with --labels/--query alone.
	dbLogin.Arg("db", "Database to retrieve credentials for. Can be obtained from 'tsh db ls' output.").StringVar(&cf.DatabaseService)
	dbLogin.Flag("labels", labelHelp).StringVar(&cf.Labels)
	dbLogin.Flag("query", queryHelp).StringVar(&cf.PredicateExpression)
	dbLogin.Flag("db-user", "Database user to configure as default.").Short('u').StringVar(&cf.DatabaseUser)
	dbLogin.Flag("db-name", "Database name to configure as default.").Short('n').StringVar(&cf.DatabaseName)
	dbLogin.Flag("db-roles", "List of comma separate database roles to use for auto-provisioned user.").Short('r').StringVar(&cf.DatabaseRoles)
	dbLogin.Flag("request-reason", "Reason for requesting access.").StringVar(&cf.RequestReason)
	dbLogin.Flag("disable-access-request", "Disable automatic resource access requests.").BoolVar(&cf.disableAccessRequest)
	dbLogout := db.Command("logout", "Remove database credentials.")
	dbLogout.Arg("db", "Database to remove credentials for.").StringVar(&cf.DatabaseService)
	dbLogout.Flag("labels", labelHelp).StringVar(&cf.Labels)
	dbLogout.Flag("query", queryHelp).StringVar(&cf.PredicateExpression)
	dbEnv := db.Command("env", "Print environment variables for the configured database.")
	dbEnv.Arg("db", "Print environment for the specified database.").StringVar(&cf.DatabaseService)
	dbEnv.Flag("format", defaults.FormatFlagDescription(defaults.DefaultFormats...)).Short('f').Default(teleport.Text).EnumVar(&cf.Format, defaults.DefaultFormats...)
	dbEnv.Flag("labels", labelHelp).StringVar(&cf.Labels)
	dbEnv.Flag("query", queryHelp).StringVar(&cf.PredicateExpression)
	// --db flag is deprecated in favor of positional argument for consistency with other commands.
	dbEnv.Flag("db", "Print environment for the specified database.").Hidden().StringVar(&cf.DatabaseService)
	dbConfig := db.Command("config", "Print database connection information. Useful when configuring GUI clients.")
	dbConfig.Arg("db", "Print information for the specified database.").StringVar(&cf.DatabaseService)
	dbConfig.Flag("labels", labelHelp).StringVar(&cf.Labels)
	dbConfig.Flag("query", queryHelp).StringVar(&cf.PredicateExpression)
	// --db flag is deprecated in favor of positional argument for consistency with other commands.
	dbConfig.Flag("db", "Print information for the specified database.").Hidden().StringVar(&cf.DatabaseService)
	dbConfig.Flag("format", fmt.Sprintf("Print format: %q to print in table format (default), %q to print connect command, %q or %q to print in JSON or YAML.",
		dbFormatText, dbFormatCommand, dbFormatJSON, dbFormatYAML)).Short('f').EnumVar(&cf.Format, dbFormatText, dbFormatCommand, dbFormatJSON, dbFormatYAML)
	dbConnect := db.Command("connect", "Connect to a database.")
	dbConnect.Arg("db", "Database service name to connect to.").StringVar(&cf.DatabaseService)
	dbConnect.Flag("db-user", "Database user to log in as.").Short('u').StringVar(&cf.DatabaseUser)
	dbConnect.Flag("db-name", "Database name to log in to.").Short('n').StringVar(&cf.DatabaseName)
	dbConnect.Flag("db-roles", "List of comma separate database roles to use for auto-provisioned user.").Short('r').StringVar(&cf.DatabaseRoles)
	dbConnect.Flag("labels", labelHelp).StringVar(&cf.Labels)
	dbConnect.Flag("query", queryHelp).StringVar(&cf.PredicateExpression)
	dbConnect.Flag("request-reason", "Reason for requesting access.").StringVar(&cf.RequestReason)
	dbConnect.Flag("disable-access-request", "Disable automatic resource access requests.").BoolVar(&cf.disableAccessRequest)
	dbConnect.Flag("tunnel", "Open authenticated tunnel using database's client certificate so clients don't need to authenticate.").Hidden().BoolVar(&cf.LocalProxyTunnel)
	dbExec := db.Command("exec", "Execute database commands on target database services.")
	dbExec.Flag("db-user", "Database user to log in as.").Short('u').StringVar(&cf.DatabaseUser)
	dbExec.Flag("db-name", "Database name to log in to.").Short('n').StringVar(&cf.DatabaseName)
	dbExec.Flag("db-roles", "List of comma separate database roles to use for auto-provisioned user.").Short('r').StringVar(&cf.DatabaseRoles)
	dbExec.Flag("search", searchHelp).StringVar(&cf.SearchKeywords)
	dbExec.Flag("labels", labelHelp).StringVar(&cf.Labels)
	dbExec.Flag("parallel", "Run commands on target databases in parallel. Defaults to 1, and maximum allowed is 10.").Default("1").IntVar(&cf.ParallelJobs)
	dbExec.Flag("output-dir", "Directory to store command output per target database service. A summary is saved as \"summary.json\".").StringVar(&cf.OutputDir)
	dbExec.Flag("dbs", "List of comma separated target database services. Mutually exclusive with --search or --labels.").StringVar(&cf.DatabaseServices)
	dbExec.Flag("confirm", "Confirm selected database services before executing command.").Default("true").BoolVar(&cf.Confirm)
	dbExec.Arg("command", "Execute this command on target database services.").Required().StringVar(&cf.DatabaseCommand)
	dbExec.Alias(dbExecHelp)

	// join
	join := app.Command("join", "Join the active SSH or Kubernetes session.")
	join.Flag("cluster", clusterHelp).Short('c').StringVar(&cf.SiteName)
	join.Flag("mode", "Mode of joining the session, valid modes are observer, moderator and peer.").Short('m').Default("observer").EnumVar(&cf.JoinMode, "observer", "moderator", "peer")
	join.Arg("session-id", "ID of the session to join.").Required().StringVar(&cf.SessionID)
	// play
	play := app.Command("play", "Replay the recorded session (SSH, Kubernetes, App, DB).")
	play.Flag("cluster", clusterHelp).Short('c').StringVar(&cf.SiteName)
	play.Flag("speed", "Playback speed, applicable when streaming SSH or Kubernetes sessions.").Default("1x").EnumVar(&cf.PlaySpeed, "0.5x", "1x", "2x", "4x", "8x")
	play.Flag("skip-idle-time", "Quickly skip over idle time, applicable when streaming SSH or Kubernetes sessions.").BoolVar(&cf.NoWait)
	play.Flag("format", defaults.FormatFlagDescription(
		teleport.PTY, teleport.JSON, teleport.YAML, teleport.Text,
	)).Short('f').Default(teleport.PTY).EnumVar(&cf.Format, teleport.PTY, teleport.JSON, teleport.YAML, teleport.Text)
	play.Arg("session-id", "ID or path to session file to play.").Required().StringVar(&cf.SessionID)

	// scp
	scp := app.Command("scp", "Transfer files to a remote SSH node.")
	scp.Flag("cluster", clusterHelp).Short('c').StringVar(&cf.SiteName)
	scp.Arg("from, to", "Source and destination to copy, one must be a local path and one must be a remote path.").Required().StringsVar(&cf.CopySpec)
	scp.Flag("recursive", "Recursive copy of subdirectories.").Short('r').BoolVar(&cf.RecursiveCopy)
	scp.Flag("port", "Port to connect to on the remote host.").Short('P').Int32Var(&cf.NodePort)
	scp.Flag("preserve", "Preserves access and modification times from the original file.").Short('p').BoolVar(&cf.PreserveAttrs)
	scp.Flag("quiet", quietHelp).Short('q').BoolVar(&cf.Quiet)
	scp.Flag("no-resume", "Disable SSH connection resumption.").Envar(noResumeEnvVar).BoolVar(&cf.DisableSSHResumption)
	scp.Flag("relogin", "Permit performing an authentication attempt on a failed command.").Default("true").BoolVar(&cf.Relogin)
	// ls
	ls := app.Command("ls", "List remote SSH nodes.")
	ls.Flag("cluster", clusterHelp).Short('c').StringVar(&cf.SiteName)
	ls.Flag("verbose", "One-line output (for text format), including node UUIDs.").Short('v').BoolVar(&cf.Verbose)
	ls.Flag("format", defaults.FormatFlagDescription(
		teleport.Text, teleport.JSON, teleport.YAML, teleport.Names,
	)).Short('f').Default(teleport.Text).EnumVar(&cf.Format, teleport.Text, teleport.JSON, teleport.YAML, teleport.Names)
	ls.Arg("labels", labelHelp).StringVar(&cf.Labels)
	ls.Flag("search", searchHelp).StringVar(&cf.SearchKeywords)
	ls.Flag("query", queryHelp).StringVar(&cf.PredicateExpression)
	ls.Flag("all", "List nodes from all clusters and proxies.").Short('R').BoolVar(&cf.ListAll)

	// clusters
	clusters := app.Command("clusters", "List available Teleport clusters.")
	clusters.Flag("format", defaults.FormatFlagDescription(defaults.DefaultFormats...)).Short('f').Default(teleport.Text).EnumVar(&cf.Format, defaults.DefaultFormats...)
	clusters.Flag("quiet", quietHelp).Short('q').BoolVar(&cf.Quiet)
	clusters.Flag("verbose", "Verbose table output, shows full label output.").Short('v').BoolVar(&cf.Verbose)

	// sessions
	sessions := app.Command("sessions", "Operate on active sessions.")
	sessionsList := sessions.Command("ls", "List active sessions.")
	sessionsList.Flag("format", defaults.FormatFlagDescription(defaults.DefaultFormats...)).Short('f').Default(teleport.Text).EnumVar(&cf.Format, defaults.DefaultFormats...)
	sessionsList.Flag("kind", "Filter by session kind(s).").Default("ssh", "k8s", "db", "app", "desktop").EnumsVar(&cf.SessionKinds, "ssh", "k8s", "kube", "db", "app", "desktop")

	// login logs in with remote proxy and obtains a "session certificate" which gets
	// stored in ~/.tsh directory
	login := app.Command("login", "Log in to a cluster and retrieve the session certificate.")
	login.Flag("out", "Identity output.").Short('o').AllowDuplicate().StringVar(&cf.IdentityFileOut)
	login.Flag("format", fmt.Sprintf("Identity format: %s, %s (for OpenSSH compatibility) or %s (for kubeconfig).",
		identityfile.DefaultFormat,
		identityfile.FormatOpenSSH,
		identityfile.FormatKubernetes,
	)).Default(string(identityfile.DefaultFormat)).Short('f').StringVar((*string)(&cf.IdentityFormat))
	login.Flag("overwrite", "Whether to overwrite the existing identity file.").BoolVar(&cf.IdentityOverwrite)
	login.Flag("request-roles", "Request one or more extra roles.").StringVar(&cf.DesiredRoles)
	login.Flag("request-reason", "Reason for requesting additional roles.").StringVar(&cf.RequestReason)
	login.Flag("request-reviewers", "Suggested reviewers for role request.").StringVar(&cf.SuggestedReviewers)
	login.Flag("request-nowait", "Finish without waiting for request resolution.").BoolVar(&cf.NoWait)
	login.Flag("request-id", "Login with the roles requested in the given request.").StringVar(&cf.RequestID)
	login.Arg("cluster", clusterHelp).StringVar(&cf.SiteName)
	login.Flag("browser", browserHelp).StringVar(&cf.Browser)
	login.Flag("kube-cluster", "Name of the Kubernetes cluster to login to.").StringVar(&cf.KubernetesCluster)
	login.Flag("verbose", "Show extra status information.").Short('v').BoolVar(&cf.Verbose)
	login.Alias(loginUsageFooter)

	// logout deletes obtained session certificates in ~/.tsh
	logout := app.Command("logout", "Delete a cluster certificate.")

	// latency
	latency := app.Command("latency", "Run latency diagnostics.").Hidden()

	latencySSH := latency.Command("ssh", "Measure latency to a particular SSH host.")
	latencySSH.Arg("[user@]host", "Remote hostname and the login to use.").Required().StringVar(&cf.UserHost)
	latencySSH.Flag("cluster", clusterHelp).Short('c').StringVar(&cf.SiteName)
	latencySSH.Flag("no-resume", "Disable SSH connection resumption.").Envar(noResumeEnvVar).BoolVar(&cf.DisableSSHResumption)

	// bench
	bench := app.Command("bench", "Run Teleport benchmark tests.").Hidden()
	bench.Flag("cluster", clusterHelp).Short('c').StringVar(&cf.SiteName)
	bench.Flag("duration", "Test duration.").Default("1s").DurationVar(&cf.BenchDuration)
	bench.Flag("rate", "Requests per second rate.").Default("10").IntVar(&cf.BenchRate)
	bench.Flag("export", "Export the latency profile.").BoolVar(&cf.BenchExport)
	bench.Flag("path", "Directory to save the latency profile to, default path is the current directory.").Default(".").StringVar(&cf.BenchExportPath)
	bench.Flag("ticks", "Ticks per half distance.").Default("100").Int32Var(&cf.BenchTicks)
	bench.Flag("scale", "Value scale in which to scale the recorded values.").Default("1.0").Float64Var(&cf.BenchValueScale)

	benchSSH := bench.Command("ssh", "Run SSH benchmark tests.").Hidden()
	benchSSH.Arg("[user@]host", "Remote hostname and the login to use.").Required().StringVar(&cf.UserHost)
	benchSSH.Arg("command", "Command to execute on a remote host.").Required().StringsVar(&cf.RemoteCommand)
	benchSSH.Flag("port", "SSH port on a remote host.").Short('p').Int32Var(&cf.NodePort)
	benchSSH.Flag("random", "Connect to random hosts for each SSH session. The provided hostname must be all: tsh bench ssh --random <user>@all <command>.").BoolVar(&cf.BenchRandom)
	benchSSH.Flag("no-resume", "Disable SSH connection resumption.").Envar(noResumeEnvVar).BoolVar(&cf.DisableSSHResumption)

	benchWeb := bench.Command("web", "Run Web benchmark tests.").Hidden()
	benchWebSSH := benchWeb.Command("ssh", "Run SSH benchmark tests.").Hidden()
	benchWebSSH.Arg("[user@]host", "Remote hostname and the login to use.").Required().StringVar(&cf.UserHost)
	benchWebSSH.Arg("command", "Command to execute on a remote host.").Required().StringsVar(&cf.RemoteCommand)
	benchWebSSH.Flag("port", "SSH port on a remote host.").Short('p').Int32Var(&cf.NodePort)
	benchWebSSH.Flag("random", "Connect to random hosts for each SSH session. The provided hostname must be all: tsh bench ssh --random <user>@all <command>.").BoolVar(&cf.BenchRandom)

	benchWebSessions := benchWeb.Command("sessions", "Run session benchmark tests.").Hidden()
	benchWebSessions.Arg("[user@]host", "Remote hostname and the login to use.").Required().StringVar(&cf.UserHost)
	benchWebSessions.Arg("command", "Command to execute on a remote host.").Required().StringsVar(&cf.RemoteCommand)
	benchWebSessions.Flag("max", "The maximum number of sessions to open. If not specified a single session per node will be opened.").IntVar(&cf.BenchMaxSessions)

	var benchKubeOpts benchKubeOptions
	benchKube := bench.Command("kube", "Run Kube benchmark tests.").Hidden()
	// kube-namespace exists for backwards compatibility.
	benchKube.Flag("kube-namespace", "Selects the Kubernetes namespace.").Default("default").Hidden().StringVar(&benchKubeOpts.namespace)
	benchKube.Flag("namespace", "Selects the Kubernetes namespace.").Default("default").StringVar(&benchKubeOpts.namespace)
	benchListKube := benchKube.Command("ls", "Run a benchmark test to list Pods.").Hidden()
	benchListKube.Arg("kube_cluster", "Kubernetes cluster to use.").Required().StringVar(&cf.KubernetesCluster)
	benchExecKube := benchKube.Command("exec", "Run a benchmark test to exec into the specified Pod.").Hidden()
	benchExecKube.Arg("kube_cluster", "Kubernetes cluster to use.").Required().StringVar(&cf.KubernetesCluster)
	benchExecKube.Arg("pod", "Pod name to exec into.").Required().StringVar(&benchKubeOpts.pod)
	benchExecKube.Arg("command", "Command to execute on a pod.").Required().StringsVar(&cf.RemoteCommand)
	benchExecKube.Flag("container", "Selects the container to exec into.").StringVar(&benchKubeOpts.container)
	benchExecKube.Flag("interactive", "Create interactive Kube session.").BoolVar(&cf.BenchInteractive)

	benchPostgres := bench.Command("postgres", "Run PostgreSQL database benchmark tests.").Hidden()
	benchPostgres.Flag("db-user", "Database user used to connect to the target database. The user must have enough permissions on the database to execute all the benchmark queries.").StringVar(&cf.DatabaseUser)
	benchPostgres.Flag("db-name", "Database name where benchmark queries will be executed.").StringVar(&cf.DatabaseName)
	benchPostgres.Arg("database", "Teleport target database name or the direct database URI. Available databases can be retrieved by running `tsh db ls`. When using direct connection, the benchmark will issue connections directly to this database, and no Teleport is involved in the testing. It must contain all the connection information, including authentication credentials.").StringVar(&cf.DatabaseService)

	benchMySQL := bench.Command("mysql", "Run MySQL database benchmark tests.").Hidden()
	benchMySQL.Flag("db-user", "Database user used to connect to the target database. The user must have enough permissions on the database to execute all the benchmark queries.").StringVar(&cf.DatabaseUser)
	benchMySQL.Flag("db-name", "Database name where benchmark queries will be executed.").StringVar(&cf.DatabaseName)
	benchMySQL.Arg("database", "Teleport target database name or the direct database URI. Available databases can be retrieved by running `tsh db ls`. When using direct connection, the benchmark will issue connections directly to this database, and no Teleport is involved in the testing. It must contain all the connection information, including authentication credentials.").StringVar(&cf.DatabaseService)

	// show key
	show := app.Command("show", "Read an identity from file and print to stdout.").Hidden()
	show.Arg("identity_file", "The file containing a public key or a certificate.").Required().StringVar(&cf.IdentityFileIn)

	// The status command shows which proxy the user is logged into and metadata
	// about the certificate.
	status := app.Command("status", "Display the list of proxy servers and retrieved certificates.")
	status.Flag("format", defaults.FormatFlagDescription(defaults.DefaultFormats...)).Short('f').Default(teleport.Text).EnumVar(&cf.Format, defaults.DefaultFormats...)
	status.Flag("verbose", "Show extra status information after successful login.").Short('v').BoolVar(&cf.Verbose)

	// The environment command prints out environment variables for the configured
	// proxy and cluster. Can be used to create sessions "sticky" to a terminal
	// even if the user runs "tsh login" again in another window.
	environment := app.Command("env", "Print commands to set Teleport session environment variables.")
	environment.Flag("format", defaults.FormatFlagDescription(defaults.DefaultFormats...)).Short('f').Default(teleport.Text).EnumVar(&cf.Format, defaults.DefaultFormats...)
	environment.Flag("unset", "Print commands to clear Teleport session environment variables.").BoolVar(&cf.unsetEnvironment)

	req := app.Command("request", "Manage access requests.").Alias("requests")

	reqList := req.Command("ls", "List access requests.").Alias("list")
	reqList.Flag("format", defaults.FormatFlagDescription(defaults.DefaultFormats...)).Short('f').Default(teleport.Text).EnumVar(&cf.Format, defaults.DefaultFormats...)
	reqList.Flag("reviewable", "Only show requests reviewable by current user.").BoolVar(&cf.ReviewableRequests)
	reqList.Flag("suggested", "Only show requests that suggest current user as reviewer.").BoolVar(&cf.SuggestedRequests)
	reqList.Flag("my-requests", "Only show requests created by current user.").BoolVar(&cf.MyRequests)

	reqShow := req.Command("show", "Show request details.").Alias("details")
	reqShow.Flag("format", defaults.FormatFlagDescription(defaults.DefaultFormats...)).Short('f').Default(teleport.Text).EnumVar(&cf.Format, defaults.DefaultFormats...)
	reqShow.Arg("request-id", "ID of the target request.").Required().StringVar(&cf.RequestID)

	// Note: The "tsh request new" subcommand should not be used anymore. It
	// will be kept around for users that built automation around it, but all
	// public facing documentation should now refer to "tsh request create".
	reqCreate := req.Command("create", "Create a new access request.").Alias("new")
	reqCreate.Flag("roles", "Roles to be requested.").StringVar(&cf.DesiredRoles)
	reqCreate.Flag("reason", "Reason for requesting.").StringVar(&cf.RequestReason)
	reqCreate.Flag("reviewers", "Suggested reviewers.").StringVar(&cf.SuggestedReviewers)
	reqCreate.Flag("nowait", "Finish without waiting for request resolution.").BoolVar(&cf.NoWait)
	reqCreate.Flag("resource", "Resource ID to be requested.").StringsVar(&cf.RequestedResourceIDs)
	reqCreate.Flag("request-ttl", "Expiration time for the access request.").DurationVar(&cf.RequestTTL)
	reqCreate.Flag("session-ttl", "Expiration time for the elevated certificate.").DurationVar(&cf.SessionTTL)
	reqCreate.Flag("max-duration", "How long the access should be granted for.").DurationVar(&cf.MaxDuration)
	reqCreate.Flag("assume-start-time", "Sets time roles can be assumed by requestor (RFC3339 e.g 2023-12-12T23:20:50.52Z).").StringVar(&cf.AssumeStartTimeRaw)

	reqReview := req.Command("review", "Review an access request.")
	reqReview.Arg("request-id", "ID of target request.").Required().StringVar(&cf.RequestID)
	reqReview.Flag("approve", "Review proposes approval.").BoolVar(&cf.Approve)
	reqReview.Flag("deny", "Review proposes denial.").BoolVar(&cf.Deny)
	reqReview.Flag("reason", "Review reason message.").StringVar(&cf.ReviewReason)
	reqReview.Flag("assume-start-time", "Sets time roles can be assumed by requestor (RFC3339 e.g 2023-12-12T23:20:50.52Z).").StringVar(&cf.AssumeStartTimeRaw)

	reqSearch := req.Command("search", "Search for resources to request access to.")
	reqSearch.Flag("kind", fmt.Sprintf("Resource kind to search for (%s).", strings.Join(types.RequestableResourceKinds, ", "))).Required().StringVar(&cf.ResourceKind)
	reqSearch.Flag("kube-kind", fmt.Sprintf("Kubernetes resource kind name (plural) to search for. Required with --kind=%q Ex: pods, deployements, namespaces, etc.", types.KindKubernetesResource)).StringVar(&cf.kubeResourceKind)
	reqSearch.Flag("kube-api-group", "Kubernetes API group to search for resources.").StringVar(&cf.kubeAPIGroup)
	reqSearch.PreAction(func(*kingpin.ParseContext) error {
		// TODO(@creack): DELETE IN v20.0.0. Allow legacy kinds with a warning for now.
		if slices.Contains(types.LegacyRequestableKubeResourceKinds, cf.ResourceKind) {
			cf.kubeAPIGroup = types.KubernetesResourcesV7KindGroups[cf.ResourceKind]
			if cf.ResourceKind == types.KindKubeNamespace {
				cf.kubeResourceKind = "namespaces"
			} else {
				cf.kubeResourceKind = types.KubernetesResourcesKindsPlurals[cf.ResourceKind]
			}
			originalKubeKind := cf.ResourceKind
			cf.ResourceKind = types.KindKubernetesResource

			nsFlag := fmt.Sprintf("--kube-namespace=%q", cf.kubeNamespace)
			if cf.kubeAllNamespaces {
				nsFlag = "--all-kube-namespaces"
			}
			fmt.Fprintf(os.Stderr, "Warning: %q is deprecated, use:\n", originalKubeKind)
			fmt.Fprintf(os.Stderr, ">tsh request search --kind=%q --kube-kind=%q --kube-api-group=%q %s\n\n", types.KindKubernetesResource, cf.kubeResourceKind, cf.kubeAPIGroup, nsFlag)
		}
		switch cf.ResourceKind {
		case types.KindKubernetesResource:
			if cf.kubeResourceKind == "" {
				return trace.BadParameter("--kube-kind is required when using --kind=%q", types.KindKubernetesResource)
			}
			if _, ok := types.KubernetesCoreResourceKinds[cf.kubeResourceKind]; !ok && cf.kubeAPIGroup == "" && cf.kubeResourceKind != types.KindKubeNamespace {
				return trace.BadParameter("--kube-api-group is required for resource kind %q", cf.kubeResourceKind)
			}
		case "":
			return trace.BadParameter("required flag --kind not provided")
		default:
			if !slices.Contains(types.RequestableResourceKinds, cf.ResourceKind) {
				return trace.BadParameter("--kind must be one of %s, got %q", strings.Join(types.RequestableResourceKinds, ", "), cf.ResourceKind)
			}
		}
		return nil
	})

	reqSearch.Flag("search", searchHelp).StringVar(&cf.SearchKeywords)
	reqSearch.Flag("query", queryHelp).StringVar(&cf.PredicateExpression)
	reqSearch.Flag("labels", labelHelp).StringVar(&cf.Labels)
	reqSearch.Flag("kube-cluster", "Kubernetes Cluster to search for Pods.").StringVar(&cf.KubernetesCluster)
	// kube-namespace exists for backwards compatibility.
	reqSearch.Flag("kube-namespace", "Kubernetes Namespace to search for Pods.").Hidden().Default(corev1.NamespaceDefault).StringVar(&cf.kubeNamespace)
	reqSearch.Flag("namespace", "Kubernetes Namespace to search for Pods.").Default(corev1.NamespaceDefault).StringVar(&cf.kubeNamespace)
	reqSearch.Flag("all-kube-namespaces", "Search Pods in every namespace.").BoolVar(&cf.kubeAllNamespaces)
	reqSearch.Flag("verbose", "Verbose table output, shows full label output.").Short('v').BoolVar(&cf.Verbose)

	// Headless login approval
	headless := app.Command("headless", "Headless authentication commands.").Interspersed(true)
	headlessApprove := headless.Command("approve", "Approve a headless authentication request.").Interspersed(true)
	headlessApprove.Arg("request id", "Headless authentication request ID.").StringVar(&cf.HeadlessAuthenticationID)
	headlessApprove.Flag("skip-confirm", "Skip confirmation and prompt for MFA immediately.").Envar(headlessSkipConfirmEnvVar).BoolVar(&cf.headlessSkipConfirm)

	reqDrop := req.Command("drop", "Drop one more access requests from current identity.")
	reqDrop.Arg("request-id", "IDs of requests to drop (default drops all requests).").Default("*").StringsVar(&cf.RequestIDs)
	kubectl := app.Command("kubectl", "Runs a kubectl command on a Kubernetes cluster.").Interspersed(false)
	// This hack is required in order to accept any args for tsh kubectl.
	kubectl.Arg("", "").StringsVar(new([]string))
	// Kubernetes subcommands.
	kube := newKubeCommand(app)
	// MFA subcommands.
	mfa := newMFACommand(app)
	// SCAN subcommands.
	scan := newScanCommand(app)

	config := app.Command("config", "Print OpenSSH configuration details.")
	config.Flag("port", "SSH port on a remote host.").Short('p').Int32Var(&cf.NodePort)

	puttyConfig := app.Command("puttyconfig", "Add PuTTY saved session configuration for specified hostname to Windows registry.")
	puttyConfig.Arg("[user@]host", "Remote hostname and optional login to use.").Required().StringVar(&cf.UserHost)
	puttyConfig.Flag("port", "SSH port on a remote host.").Short('p').Int32Var(&cf.NodePort)
	puttyConfig.Flag("leaf", "Add a configuration for connecting to a leaf cluster.").StringVar(&cf.LeafClusterName)
	// only expose `tsh puttyconfig` subcommand on windows
	if runtime.GOOS != constants.WindowsOS {
		puttyConfig.Hidden()
	}

	// Client-tools managed updates commands.
	updateCommand := newUpdateCommand(app)

	// FIDO2, TouchID and WebAuthnWin commands.
	f2 := fido2.NewCommand(app)
	tid := touchid.NewCommand(app)
	wanwin := webauthnwin.NewCommand(app)

	// Device Trust commands.
	deviceCmd := newDeviceCommand(app)

	svidCmd := newSVIDCommands(app)
	workloadIdentityCmd := newWorkloadIdentityCommands(app)

	vnetCommand := newVnetCommand(app)
	vnetSSHAutoConfigCommand := newVnetSSHAutoConfigCommand(app)
	vnetAdminSetupCommand := newVnetAdminSetupCommand(app)
	vnetDaemonCommand := newVnetDaemonCommand(app)
	vnetServiceCommand := newVnetServiceCommand(app)
	vnetInstallServiceCommand := newVnetInstallServiceCommand(app)
	vnetUninstallServiceCommand := newVnetUninstallServiceCommand(app)

	gitCmd := newGitCommands(app)
	pivCmd := newPIVCommands(app)
	mcpCmd := newMCPCommands(app, &cf)

	if runtime.GOOS == constants.WindowsOS {
		bench.Hidden()
	}

	var err error
	cf.executablePath, err = os.Executable()
	if err != nil {
		return trace.Wrap(err)
	}

	// configs
	setEnvFlags(&cf)

	confOptions, err := client.LoadAllConfigs(cf.GlobalTshConfigPath, cf.HomePath)
	if err != nil {
		return trace.Wrap(err)
	}
	cf.TSHConfig = *confOptions

	// aliases
	ar := newAliasRunner(cf.TSHConfig.Aliases)
	aliasCommand, runtimeArgs := findAliasCommand(args)
	if aliasDefinition, ok := ar.getAliasDefinition(aliasCommand); ok {
		return ar.runAlias(ctx, aliasCommand, aliasDefinition, cf.executablePath, runtimeArgs)
	}

	// parse CLI commands+flags:
	utils.UpdateAppUsageTemplate(app, args)
	command, err := app.Parse(args)
	if errors.Is(err, kingpin.ErrExpectedCommand) {
		if _, ok := cf.TSHConfig.Aliases[aliasCommand]; ok {
			logger.DebugContext(ctx, "Failing due to recursive alias",
				"alias", aliasCommand,
				"aliases_seen", ar.getSeenAliases(),
			)
			return trace.BadParameter("recursive alias %q; correct alias definition and try again", aliasCommand)
		}
	}

	// Handle fork after authentication.
	if cf.ForkAfterAuthentication && !cf.isForkAuthChild() {
		if len(cf.RemoteCommand) == 0 {
			return trace.BadParameter("fork after authentication not allowed for interactive sessions")
		}
		forkParams := reexec.ForkAuthenticateParams{
			GetArgs: func(signalFd, killFd uint64) []string {
				return append([]string{
					// fd flags go immediately after `tsh`.
					"--fork-signal-fd", strconv.FormatUint(signalFd, 10),
					"--fork-kill-fd", strconv.FormatUint(killFd, 10),
				}, args...)
			},
			Stdin:  cf.Stdin(),
			Stdout: cf.Stdout(),
			Stderr: cf.Stderr(),
		}
		if err := reexec.RunForkAuthenticate(cf.Context, forkParams); err != nil {
			var execErr *exec.ExitError
			if errors.As(trace.Unwrap(err), &execErr) {
				err = &common.ExitCodeError{Code: execErr.ExitCode()}
			}
			return trace.Wrap(err)
		}
		return nil
	}

	// Remove HTTPS:// in proxy parameter as https is automatically added
	cf.Proxy = strings.TrimPrefix(cf.Proxy, "https://")
	cf.Proxy = strings.TrimPrefix(cf.Proxy, "HTTPS://")

	// Identity files do not currently contain a proxy address. When loading an
	// Identity file, a proxy must be passed on the command line as well.
	if cf.IdentityFileIn != "" && cf.Proxy == "" {
		return trace.BadParameter("tsh --identity also requires --proxy")
	}

	// prevent Kingpin from calling os.Exit(), we want to handle errors ourselves.
	// shouldTerminate will be checked after app.Parse() call.
	var shouldTerminate *int
	app.Terminate(func(exitCode int) {
		// make non-zero exit code sticky
		if exitCode == 0 && shouldTerminate != nil {
			return
		}
		shouldTerminate = &exitCode
	})

	if err != nil {
		app.Usage(args)
		return trace.Wrap(err)
	}

	// handle: help command, --help flag, version command, ...
	if shouldTerminate != nil {
		return trace.Wrap(&common.ExitCodeError{Code: *shouldTerminate})
	}

	// Did we initially get the Username from flags/env?
	cf.ExplicitUsername = cf.Username != ""

	cf.command = command
	// Convert --disable-access-request for compatibility.
	if cf.disableAccessRequest {
		cf.RequestMode = accessRequestModeOff
	}

	// apply any options after parsing of arguments to ensure
	// that defaults don't overwrite options.
	for _, opt := range opts {
		if err := opt(&cf); err != nil {
			return trace.Wrap(err)
		}
	}

	// Enable debug logging if requested by --debug.
	// If TELEPORT_DEBUG was set and --debug/--no-debug was not passed, debug logs were already
	// enabled by a prior call to initLogger.
	if _, err := initLogger(&cf, utils.LoggingForCLI, parseLoggingOptsFromEnvAndArgv(&cf)); err != nil {
		printInitLoggerError(err)
	}

	stopTracing := initializeTracing(&cf)
	defer stopTracing()

	// start the span for the command and update the config context so that all spans created
	// in the future will be rooted at this span.
	ctx, span := cf.tracer.Start(cf.Context, command)
	cf.Context = ctx
	defer span.End()

	if err := client.ValidateAgentKeyOption(cf.AddKeysToAgent); err != nil {
		return trace.Wrap(err)
	}

	if cpuProfile != "" {
		logger.DebugContext(ctx, "writing CPU profile", "file", cpuProfile)
		f, err := os.Create(cpuProfile)
		if err != nil {
			return trace.Wrap(err)
		}
		defer f.Close()
		if err := pprof.StartCPUProfile(f); err != nil {
			return trace.Wrap(err)
		}
		defer pprof.StopCPUProfile()
	}

	if memProfile != "" {
		logger.DebugContext(ctx, "writing memory profile", "file", memProfile)
		defer func() {
			f, err := os.Create(memProfile)
			if err != nil {
				logger.ErrorContext(ctx, "could not open memory profile", "error", err)
				return
			}
			defer f.Close()
			runtime.GC()
			if err := pprof.WriteHeapProfile(f); err != nil {
				logger.ErrorContext(ctx, "could not write memory profile", "error", err)
				return
			}
		}()
	}

	if traceProfile != "" {
		logger.DebugContext(ctx, "writing trace profile", "file", traceProfile)
		f, err := os.Create(traceProfile)
		if err != nil {
			return trace.Wrap(err)
		}
		defer f.Close()

		if err := runtimetrace.Start(f); err != nil {
			return trace.Wrap(err)
		}
		defer runtimetrace.Stop()
	}

	switch command {
	case ver.FullCommand():
		err = onVersion(&cf)
	case ssh.FullCommand():
		err = onSSH(&cf, wrapInitClientWithUpdateCheck(makeClient, args))
	case resolve.FullCommand():
		err = onResolve(&cf)
		// If quiet was specified for this command and
		// an error occurred, exit with a non-zero exit
		// code without emitting any other messaging.
		// In this case, the command was likely invoked
		// via a Match exec block from an SSH config and
		// if no matches were found, we should not add
		// additional spam to stderr.
		if err != nil && cf.Quiet {
			err = trace.Wrap(&common.ExitCodeError{Code: 1})
		}
	case latencySSH.FullCommand():
		err = onSSHLatency(&cf)
	case benchSSH.FullCommand():
		err = onBenchmark(
			&cf,
			&benchmark.SSHBenchmark{
				Command: cf.RemoteCommand,
				Random:  cf.BenchRandom,
			},
		)
	case benchWebSSH.FullCommand():
		err = onBenchmark(
			&cf,
			&benchmark.WebSSHBenchmark{
				Command:  cf.RemoteCommand,
				Random:   cf.BenchRandom,
				Duration: cf.BenchDuration,
			},
		)
	case benchWebSessions.FullCommand():
		err = onBenchmark(
			&cf,
			&benchmark.WebSessionBenchmark{
				Command:  cf.RemoteCommand,
				Max:      cf.BenchMaxSessions,
				Duration: cf.BenchDuration,
			},
		)
	case benchListKube.FullCommand():
		err = onBenchmark(
			&cf,
			&benchmark.KubeListBenchmark{
				Namespace: benchKubeOpts.namespace,
			},
		)
	case benchExecKube.FullCommand():
		err = onBenchmark(
			&cf,
			&benchmark.KubeExecBenchmark{
				Command:       cf.RemoteCommand,
				Namespace:     benchKubeOpts.namespace,
				PodName:       benchKubeOpts.pod,
				ContainerName: benchKubeOpts.container,
				Interactive:   cf.BenchInteractive,
			},
		)
	case benchPostgres.FullCommand():
		err = onBenchmark(
			&cf,
			&benchmarkdb.PostgresBenchmark{
				DBService:          cf.DatabaseService,
				DBUser:             cf.DatabaseUser,
				DBName:             cf.DatabaseName,
				InsecureSkipVerify: cf.InsecureSkipVerify,
			},
		)
	case benchMySQL.FullCommand():
		err = onBenchmark(
			&cf,
			&benchmarkdb.MySQLBenchmark{
				DBService:          cf.DatabaseService,
				DBUser:             cf.DatabaseUser,
				DBName:             cf.DatabaseName,
				InsecureSkipVerify: cf.InsecureSkipVerify,
			},
		)
	case join.FullCommand():
		err = onJoin(&cf)
	case scp.FullCommand():
		err = onSCP(&cf)
	case play.FullCommand():
		err = onPlay(&cf)
	case ls.FullCommand():
		err = onListNodes(&cf)
	case clusters.FullCommand():
		err = onListClusters(&cf)
	case sessionsList.FullCommand():
		err = onListSessions(&cf)
	case login.FullCommand():
		err = onLogin(&cf, args...)
	case logout.FullCommand():
		err = onLogout(&cf)
	case show.FullCommand():
		err = onShow(&cf)
	case status.FullCommand():
		// onStatus can be invoked directly with `tsh status` but is also
		// invoked from other commands. When invoked directly, we use a
		// context with a short timeout to prevent the command from taking
		// too long due to fetching alerts on slow networks.
		var cancel context.CancelFunc
		cf.Context, cancel = context.WithTimeout(cf.Context, constants.TimeoutGetClusterAlerts)
		defer cancel()
		err = onStatus(&cf)
	case lsApps.FullCommand():
		err = onApps(&cf)
	case lsRecordings.FullCommand():
		err = onRecordings(&cf)
	case exportRecordings.FullCommand():
		err = onExportRecording(&cf)
	case appLogin.FullCommand():
		err = onAppLogin(&cf)
	case appLogout.FullCommand():
		err = onAppLogout(&cf)
	case appConfig.FullCommand():
		err = onAppConfig(&cf)
	case kube.credentials.FullCommand():
		err = kube.credentials.run(&cf)
	case kube.ls.FullCommand():
		err = kube.ls.run(&cf)
	case kube.login.FullCommand():
		err = kube.login.run(&cf)
	case kube.sessions.FullCommand():
		err = kube.sessions.run(&cf)
	case kube.exec.FullCommand():
		err = kube.exec.run(&cf)
	case kube.join.FullCommand():
		err = kube.join.run(&cf)
	case scan.keys.FullCommand():
		err = scan.keys.run(&cf)
	case proxySSH.FullCommand():
		err = onProxyCommandSSH(&cf, wrapInitClientWithUpdateCheck(makeClient, args))
	case proxyDB.FullCommand():
		err = onProxyCommandDB(&cf)
	case proxyApp.FullCommand():
		err = onProxyCommandApp(&cf)
	case proxyAWS.FullCommand():
		err = onProxyCommandAWS(&cf)
	case proxyAzure.FullCommand():
		err = onProxyCommandAzure(&cf)
	case proxyGcloud.FullCommand():
		err = onProxyCommandGCloud(&cf)
	case proxyKube.FullCommand():
		err = proxyKube.run(&cf)

	case dbList.FullCommand():
		err = onListDatabases(&cf)
	case dbLogin.FullCommand():
		err = onDatabaseLogin(&cf)
	case dbLogout.FullCommand():
		err = onDatabaseLogout(&cf)
	case dbEnv.FullCommand():
		err = onDatabaseEnv(&cf)
	case dbConfig.FullCommand():
		err = onDatabaseConfig(&cf)
	case dbConnect.FullCommand():
		err = onDatabaseConnect(&cf)
	case dbExec.FullCommand():
		err = onDatabaseExec(&cf)
	case environment.FullCommand():
		err = onEnvironment(&cf)
	case mfa.ls.FullCommand():
		err = mfa.ls.run(&cf)
	case mfa.add.FullCommand():
		err = mfa.add.run(&cf)
	case mfa.rm.FullCommand():
		err = mfa.rm.run(&cf)
	case reqList.FullCommand():
		err = onRequestList(&cf)
	case reqShow.FullCommand():
		err = onRequestShow(&cf)
	case reqCreate.FullCommand():
		err = onRequestCreate(&cf)
	case reqReview.FullCommand():
		err = onRequestReview(&cf)
	case reqSearch.FullCommand():
		err = onRequestSearch(&cf)
	case reqDrop.FullCommand():
		err = onRequestDrop(&cf)
	case config.FullCommand():
		err = onConfig(&cf)
	case puttyConfig.FullCommand():
		err = onPuttyConfig(&cf)
	case aws.FullCommand():
		err = onAWS(&cf)
	case azure.FullCommand():
		err = onAzure(&cf)
	case gcloud.FullCommand():
		err = onGcloud(&cf)
	case gsutil.FullCommand():
		err = onGsutil(&cf)
	case daemonStart.FullCommand():
		err = onDaemonStart(&cf)
	case daemonStop.FullCommand():
		err = onDaemonStop(&cf)
	case f2.Diag.FullCommand():
		err = f2.Diag.Run(cf.Context)
	case f2.Attobj.FullCommand():
		err = f2.Attobj.Run()
	case tid.Diag.FullCommand():
		err = tid.Diag.Run()
	case wanwin.Diag.FullCommand():
		err = wanwin.Diag.Run(cf.Context)
	case deviceCmd.enroll.FullCommand():
		err = deviceCmd.enroll.run(&cf)
	case deviceCmd.collect.FullCommand():
		err = deviceCmd.collect.run(&cf)
	case deviceCmd.assetTag.FullCommand():
		err = deviceCmd.assetTag.run(&cf)
	case deviceCmd.keyget.FullCommand():
		err = deviceCmd.keyget.run(&cf)
	case deviceCmd.activateCredential.FullCommand():
		err = deviceCmd.activateCredential.run(&cf)
	case deviceCmd.dmiRead.FullCommand():
		err = deviceCmd.dmiRead.run(&cf)
	case kubectl.FullCommand():
		idx := slices.Index(args, kubectl.FullCommand())
		err = onKubectlCommand(&cf, args, args[idx:])
	case headlessApprove.FullCommand():
		err = onHeadlessApprove(&cf)
	case svidCmd.issue.FullCommand():
		err = svidCmd.issue.run(&cf)
	case workloadIdentityCmd.issueX509.FullCommand():
		err = workloadIdentityCmd.issueX509.run(&cf)
	case vnetCommand.FullCommand():
		err = vnetCommand.run(&cf)
	case vnetSSHAutoConfigCommand.FullCommand():
		err = vnetSSHAutoConfigCommand.run(&cf)
	case vnetAdminSetupCommand.FullCommand():
		err = vnetAdminSetupCommand.run(&cf)
	case vnetDaemonCommand.FullCommand():
		err = vnetDaemonCommand.run(&cf)
	case vnetServiceCommand.FullCommand():
		err = vnetServiceCommand.run(&cf)
	case vnetInstallServiceCommand.FullCommand():
		err = vnetInstallServiceCommand.run(&cf)
	case vnetUninstallServiceCommand.FullCommand():
		err = vnetUninstallServiceCommand.run(&cf)
	case gitCmd.list.FullCommand():
		err = gitCmd.list.run(&cf)
	case gitCmd.login.FullCommand():
		err = gitCmd.login.run(&cf)
	case gitCmd.ssh.FullCommand():
		err = gitCmd.ssh.run(&cf)
	case gitCmd.config.FullCommand():
		err = gitCmd.config.run(&cf)
	case gitCmd.clone.FullCommand():
		err = gitCmd.clone.run(&cf)
	case pivCmd.agent.FullCommand():
		err = pivCmd.agent.run(&cf)
	case mcpCmd.dbStart.FullCommand():
		err = mcpCmd.dbStart.run()
	case mcpCmd.dbConfig.FullCommand():
		err = mcpCmd.dbConfig.run()
	case mcpCmd.connect.FullCommand():
		err = mcpCmd.connect.run()
	case mcpCmd.list.FullCommand():
		err = mcpCmd.list.run()
	case mcpCmd.config.FullCommand():
		err = mcpCmd.config.run()
	case updateCommand.update.FullCommand():
		err = updateCommand.update.run(&cf)
	default:
		// Handle commands that might not be available.
		switch {
		case tid.Ls.MatchesCommand(command):
			err = tid.Ls.Run()
		case tid.Rm.MatchesCommand(command):
			err = tid.Rm.Run()
		default:
			// This should only happen when there's a missing switch case above.
			err = trace.BadParameter("command %q not configured", command)
		}
	}

	if trace.IsNotImplemented(err) {
		return handleUnimplementedError(ctx, err, cf)
	}

	return trace.Wrap(err)
}

func initializeTracing(cf *CLIConf) func() {
	cf.TracingProvider = tracing.NoopProvider()
	cf.tracer = cf.TracingProvider.Tracer(teleport.ComponentTSH)

	// flush ensures that the spans are all attempted to be written when tsh exits.
	flush := func(provider *tracing.Provider) func() {
		return func() {
			shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(cf.Context), time.Second)
			defer cancel()
			err := provider.Shutdown(shutdownCtx)
			if err != nil && !errors.Is(err, context.DeadlineExceeded) {
				logger.DebugContext(shutdownCtx, "failed to shutdown trace provider", "error", err)
			}
		}
	}

	// A default sampling rate of 1 ensures that all spans for this invocation of
	// tsh are guaranteed to be recorded. Since Teleport honors the sampling rate
	// of remote spans this will also cause Teleport to sample any spans it generates
	// in response to the client request.
	const samplingRate = 1.0

	switch {
	// kubectl is a special case because it is the only command that we re-execute
	// in order to be able to access the exit code and stdout/stderr of the command
	// that was run and determine if we should create a new access request from
	// the output data.
	// We don't want to enable tracing for the master invocation of tsh kubectl
	// because the data that we would be tracing would be the tsh kubectl command.
	// Instead, we want to enable tracing for the re-executed kubectl command and
	// we do that in the kubectl command handler.
	case cf.command == "kubectl":
		return func() {}
	// The user explicitly asked for traces to be sent to a particular exporter
	// instead of forwarding them to Auth. Proceed with creating the provider.
	case cf.SampleTraces && cf.TraceExporter != "":
		provider, err := tracing.NewTraceProvider(cf.Context, tracing.Config{
			Service:      teleport.ComponentTSH,
			ExporterURL:  cf.TraceExporter,
			SamplingRate: samplingRate,
		})
		if err != nil {
			logger.DebugContext(cf.Context, "failed to connect to trace exporter",
				"error", err,
				"exporter", cf.TraceExporter,
			)
			return func() {}
		}

		cf.TracingProvider = provider
		cf.tracer = provider.Tracer(teleport.ComponentTSH)
		return flush(provider)
	// The login command cannot forward spans to Auth since there is no way to get
	// an authenticated client to forward with until after the authentication ceremony
	// is complete. However, if the user explicitly provided an exporter then the login
	// spans can be sent directly to it.
	case cf.command == "login":
		return func() {}
	// All commands besides ssh are only traced if the user explicitly requested
	// tracing. For ssh, a random number of spans may be sampled if the Proxy is
	// for a Cloud tenant.
	case !cf.SampleTraces:
		return func() {}
	case cf.SampleTraces:
	}

	// Parse the config to determine if forwarding is needed for Cloud and
	// to get a handle to an Auth client.
	tc, err := makeClient(cf)
	if err != nil {
		logger.DebugContext(cf.Context, "failed to set up span forwarding", "error", err)
		return func() {}
	}

	var provider *tracing.Provider
	if err := client.RetryWithRelogin(cf.Context, tc, func() error {
		clt, err := tc.NewTracingClient(cf.Context)
		if err != nil {
			return trace.Wrap(err)
		}

		p, err := tracing.NewTraceProvider(cf.Context,
			tracing.Config{
				Service:      teleport.ComponentTSH,
				Client:       clt,
				SamplingRate: samplingRate,
			})
		if err != nil {
			return trace.NewAggregate(err, clt.Close())
		}

		provider = p
		return nil
	}); err != nil {
		logger.DebugContext(cf.Context, "failed to set up span forwarding", "error", err)
		return func() {}
	}

	cf.TracingProvider = provider
	cf.tracer = provider.Tracer(teleport.ComponentTSH)
	return flush(provider)
}

// onVersion prints version info.
func onVersion(cf *CLIConf) error {
	proxyVersion := ""
	proxyPublicAddr := ""
	// Check proxy version if not in client only mode
	if !cf.clientOnlyVersionCheck {
		pv, ppa, err := fetchProxyVersion(cf)
		if err != nil {
			fmt.Fprintf(cf.Stderr(), "Failed to fetch proxy version: %s\n", err)
		}
		proxyVersion = pv
		proxyPublicAddr = ppa
	}

	reExecFromVersion := tools.GetReExecFromVersion(cf.Context)
	format := strings.ToLower(cf.Format)
	switch format {
	case teleport.Text, "":
		modules.GetModules().PrintVersion()
		if proxyVersion != "" {
			fmt.Printf("Proxy version: %s\n", proxyVersion)
			fmt.Printf("Proxy: %s\n", proxyPublicAddr)
		}
		if reExecFromVersion != "" {
			fmt.Printf("Re-executed from version: %s\n", reExecFromVersion)
		}
	case teleport.JSON, teleport.YAML:
		out, err := serializeVersion(format, proxyVersion, proxyPublicAddr, reExecFromVersion)
		if err != nil {
			return trace.Wrap(err)
		}
		fmt.Println(out)
	default:
		return trace.BadParameter("unsupported format %q", cf.Format)
	}

	return nil
}

// fetchProxyVersion returns the current version of the Teleport Proxy.
func fetchProxyVersion(cf *CLIConf) (string, string, error) {
	profile, err := cf.ProfileStatus()
	if err != nil {
		if trace.IsNotFound(err) {
			return "", "", nil
		}
		return "", "", trace.Wrap(err)
	}

	if profile == nil {
		return "", "", nil
	}

	tc, err := makeClient(cf)
	if err != nil {
		return "", "", trace.Wrap(err)
	}

	ctx, cancel := context.WithTimeout(cf.Context, time.Second*5)
	defer cancel()
	pingRes, err := tc.Ping(ctx)
	if err != nil {
		return "", "", trace.Wrap(err)
	}

	return pingRes.ServerVersion, pingRes.Proxy.SSH.PublicAddr, nil
}

type benchKubeOptions struct {
	pod       string
	container string
	namespace string
}

func serializeVersion(format string, proxyVersion string, proxyPublicAddress string, reExecFromVersion string) (string, error) {
	versionInfo := struct {
		Version               string `json:"version"`
		Gitref                string `json:"gitref"`
		Runtime               string `json:"runtime"`
		ProxyVersion          string `json:"proxyVersion,omitempty"`
		ProxyPublicAddress    string `json:"proxyPublicAddress,omitempty"`
		ReExecutedFromVersion string `json:"reExecutedFromVersion,omitempty"`
	}{
		teleport.Version,
		teleport.Gitref,
		runtime.Version(),
		proxyVersion,
		proxyPublicAddress,
		reExecFromVersion,
	}
	var out []byte
	var err error
	if format == teleport.JSON {
		out, err = utils.FastMarshalIndent(versionInfo, "", "  ")
	} else {
		out, err = yaml.Marshal(versionInfo)
	}
	return string(out), trace.Wrap(err)
}

// onLogin logs in with remote proxy and gets signed certificates
func onLogin(cf *CLIConf, reExecArgs ...string) error {
	autoRequest := true
	// special case: --request-roles=no disables auto-request behavior.
	if cf.DesiredRoles == "no" {
		autoRequest = false
		cf.DesiredRoles = ""
	}

	// For login operations, we use the hardware key
	// service directly instead of the agent.
	cf.disableHardwareKeyAgentClient = true

	if cf.IdentityFileIn != "" {
		err := flattenIdentity(cf)
		if err != nil {
			return trace.Wrap(err, "converting identity file into a local profile")
		}
		return nil
	}

	switch cf.IdentityFormat {
	case identityfile.FormatFile, identityfile.FormatOpenSSH, identityfile.FormatKubernetes:
	default:
		return trace.BadParameter("invalid identity format: %s", cf.IdentityFormat)
	}

	// Get the status of the active profile as well as the status
	// of any other proxies the user is logged into.
	profile, profiles, err := cf.FullProfileStatus()
	if err != nil {
		if !trace.IsNotFound(err) {
			return trace.Wrap(err)
		}
	}

	// make the teleport client and retrieve the certificate from the proxy:
	tc, err := makeClient(cf)
	if err != nil {
		return trace.Wrap(err)
	}

	// The user is not logged in and has typed in `tsh --proxy=... login`, if
	// the running binary needs to be updated, update and re-exec.
	if profile == nil {
		if err := tools.CheckAndUpdateRemote(cf.Context, tc.WebProxyAddr, tc.InsecureSkipVerify, reExecArgs); err != nil {
			return trace.Wrap(err)
		}
	}

	// client is already logged in and profile is not expired
	if profile != nil && !profile.IsExpired(time.Now()) {
		switch {
		// in case if nothing is specified, re-fetch kube clusters and print
		// current status
		//   OR
		// in case if parameters match, re-fetch kube clusters and print
		// current status
		case cf.Proxy == "" && cf.SiteName == "" && cf.DesiredRoles == "" && cf.RequestID == "" && cf.IdentityFileOut == "" ||
			utils.TryHost(cf.Proxy) == utils.TryHost(profile.ProxyURL.Host) && cf.SiteName == profile.Cluster && cf.DesiredRoles == "" && cf.RequestID == "":

			// The user has typed `tsh login`, if the running binary needs to
			// be updated, update and re-exec.
			if err := tools.CheckAndUpdateRemote(cf.Context, tc.WebProxyAddr, tc.InsecureSkipVerify, reExecArgs); err != nil {
				return trace.Wrap(err)
			}

			_, err := tc.PingAndShowMOTD(cf.Context)
			if err != nil {
				return trace.Wrap(err)
			}
			if err := updateKubeConfigOnLogin(cf, tc); err != nil {
				return trace.Wrap(err)
			}

			return trace.Wrap(printLoginInformation(cf, profile, profiles, cf.getAccessListsToReview(tc)))

		// if the proxy names match but nothing else is specified; show motd and update active profile and kube configs
		case utils.TryHost(cf.Proxy) == utils.TryHost(profile.ProxyURL.Host) &&
			cf.SiteName == "" && cf.DesiredRoles == "" && cf.RequestID == "" && cf.IdentityFileOut == "":

			// The user has typed `tsh login`, if the running binary needs to
			// be updated, update and re-exec.
			if err := tools.CheckAndUpdateRemote(cf.Context, tc.WebProxyAddr, tc.InsecureSkipVerify, reExecArgs); err != nil {
				return trace.Wrap(err)
			}

			_, err := tc.PingAndShowMOTD(cf.Context)
			if err != nil {
				return trace.Wrap(err)
			}

			if err := tc.SaveProfile(true); err != nil {
				return trace.Wrap(err)
			}

			// Try updating kube config. If it fails, then we may have
			// switched to an inactive profile. Continue to normal login.
			if err := updateKubeConfigOnLogin(cf, tc); err == nil {
				profile, profiles, err = cf.FullProfileStatus()
				if err != nil {
					return trace.Wrap(err)
				}

				// Print status to show information of the logged in user.
				return trace.Wrap(printLoginInformation(cf, profile, profiles, cf.getAccessListsToReview(tc)))
			}

		// proxy is unspecified or the same as the currently provided proxy,
		// but cluster is specified, treat this as selecting a new cluster
		// for the same proxy
		case (cf.Proxy == "" || utils.TryHost(cf.Proxy) == utils.TryHost(profile.ProxyURL.Host)) && cf.SiteName != "":
			_, err := tc.PingAndShowMOTD(cf.Context)
			if err != nil {
				return trace.Wrap(err)
			}
			// trigger reissue, preserving any active requests.
			err = tc.ReissueUserCerts(cf.Context, client.CertCacheKeep, client.ReissueParams{
				AccessRequests: profile.ActiveRequests,
				RouteToCluster: cf.SiteName,
			})
			if err != nil {
				return trace.Wrap(err)
			}
			if err := tc.SaveProfile(true); err != nil {
				return trace.Wrap(err)
			}
			if err := updateKubeConfigOnLogin(cf, tc); err != nil {
				return trace.Wrap(err)
			}

			profile, profiles, err = cf.FullProfileStatus()
			if err != nil {
				return trace.Wrap(err)
			}

			// Print status to show information of the logged in user.
			return trace.Wrap(printLoginInformation(cf, profile, profiles, cf.getAccessListsToReview(tc)))
		// proxy is unspecified or the same as the currently provided proxy,
		// but desired roles or request ID is specified, treat this as a
		// privilege escalation request for the same login session.
		case (cf.Proxy == "" || utils.TryHost(cf.Proxy) == utils.TryHost(profile.ProxyURL.Host)) && (cf.DesiredRoles != "" || cf.RequestID != "") && cf.IdentityFileOut == "":
			_, err := tc.PingAndShowMOTD(cf.Context)
			if err != nil {
				return trace.Wrap(err)
			}
			if err := executeAccessRequest(cf, tc); err != nil {
				return trace.Wrap(err)
			}
			if err := updateKubeConfigOnLogin(cf, tc); err != nil {
				return trace.Wrap(err)
			}
			// Print status to show information of the logged in user.
			return trace.Wrap(printLoginInformation(cf, profile, profiles, cf.getAccessListsToReview(tc)))

		// otherwise just pass through to standard login
		default:
			// The user is logged in and has typed in `tsh --proxy=... login`, if
			// the running binary needs to be updated, update and re-exec.
			if err := tools.CheckAndUpdateRemote(cf.Context, tc.WebProxyAddr, tc.InsecureSkipVerify, reExecArgs); err != nil {
				return trace.Wrap(err)
			}
		}
	}

	// If the cluster is using single-sign on, providing the user name with --user
	// is likely a mistake, so display a warning.
	if cf.Username != "" && !slices.Contains(constants.LocalConnectors, cf.AuthConnector) {
		pr, err := tc.Ping(cf.Context)
		if err != nil {
			return trace.Wrap(err, "Teleport proxy not available at %s.", tc.WebProxyAddr)
		}
		if !slices.Contains(constants.LocalConnectors, pr.Auth.Type) {
			fmt.Fprintf(os.Stderr, "WARNING: Ignoring Teleport user (%v) for Single Sign-On (SSO) login.\nProvide the user name during the SSO flow instead. Use --auth=local if you did not intend to login with SSO.\n", cf.Username)
		}
	}

	if cf.Username == "" {
		cf.Username = tc.Username
	}

	// stdin hijack is OK for login, since it tsh doesn't read input after the
	// login ceremony is complete.
	// Only allow the option during the login ceremony.
	tc.AllowStdinHijack = true

	keyRing, err := tc.Login(cf.Context)
	if err != nil {
		if !cf.ExplicitUsername && authclient.IsInvalidLocalCredentialError(err) {
			fmt.Fprintf(os.Stderr, "\nhint: set the --user flag to log in as a specific user, or leave it empty to use the system user (%v)\n\n", tc.Username)
		}
		return trace.Wrap(err)
	}

	tc.AllowStdinHijack = false

	// the login operation may update the username and should be considered the more
	// "authoritative" source.
	cf.Username = tc.Username

	clusterClient, rootAuthClient, err := tc.ConnectToRootCluster(cf.Context, keyRing)
	if err != nil {
		return trace.Wrap(err)
	}
	defer func() {
		rootAuthClient.Close()
		clusterClient.Close()
	}()

	// TODO(fspmarshall): Refactor access request & cert reissue logic to allow
	// access requests to be applied to identity files.
	if cf.IdentityFileOut != "" {
		// keyRing.TrustedCA at this point only has the CA of the root cluster we
		// logged into. We need to fetch all the CAs for leaf clusters too, to
		// make them available in the identity file.
		authorities, err := rootAuthClient.GetCertAuthorities(cf.Context, types.HostCA, false)
		if err != nil {
			return trace.Wrap(err)
		}
		keyRing.TrustedCerts = authclient.AuthoritiesToTrustedCerts(authorities)
		// If we're in multiplexed mode get SNI name for kube from single multiplexed proxy addr
		kubeTLSServerName := ""
		if tc.TLSRoutingEnabled {
			logger.DebugContext(cf.Context, "Using Proxy SNI for kube TLS server name")
			kubeHost, _ := tc.KubeProxyHostPort()
			kubeTLSServerName = client.GetKubeTLSServerName(kubeHost)
		}
		filesWritten, err := identityfile.Write(cf.Context, identityfile.WriteConfig{
			OutputPath:           cf.IdentityFileOut,
			KeyRing:              keyRing,
			Format:               cf.IdentityFormat,
			KubeProxyAddr:        tc.KubeClusterAddr(),
			OverwriteDestination: cf.IdentityOverwrite,
			KubeStoreAllCAs:      tc.LoadAllCAs,
			KubeTLSServerName:    kubeTLSServerName,
			KubeClusterName:      tc.KubernetesCluster,
		})
		if err != nil {
			return trace.Wrap(err)
		}
		fmt.Printf("\nThe certificate has been written to %s\n", strings.Join(filesWritten, ","))
		return nil
	}

	// Attempt device login. This activates a fresh keyRing if successful.
	// We do not save the resulting in the identity file above on purpose, as this
	// certificate is bound to the present device.
	if err := tc.AttemptDeviceLogin(cf.Context, keyRing, rootAuthClient); err != nil {
		return trace.Wrap(err)
	}

	// If the proxy is advertising that it supports Kubernetes, update kubeconfig.
	if tc.KubeProxyAddr != "" {
		if err := updateKubeConfigOnLogin(cf, tc); err != nil {
			return trace.Wrap(err)
		}
	}

	// Regular login without -i flag.
	if err := tc.SaveProfile(true); err != nil {
		return trace.Wrap(err)
	}

	if autoRequest && cf.DesiredRoles == "" && cf.RequestID == "" {
		capabilities, err := rootAuthClient.GetAccessCapabilities(cf.Context, types.AccessCapabilitiesRequest{
			User: cf.Username,
		})
		if err != nil {
			logoutErr := tc.Logout()
			return trace.NewAggregate(err, logoutErr)
		}
		if capabilities.RequireReason && cf.RequestReason == "" {
			msg := "--request-reason must be specified"
			if capabilities.RequestPrompt != "" {
				msg = msg + ", prompt=" + capabilities.RequestPrompt
			}
			err := trace.BadParameter("%s", msg)
			logoutErr := tc.Logout()
			return trace.NewAggregate(err, logoutErr)
		}
		if capabilities.AutoRequest {
			cf.DesiredRoles = "*"
		}
	}

	if cf.DesiredRoles != "" || cf.RequestID != "" {
		fmt.Println("") // visually separate access request output
		if err := executeAccessRequest(cf, tc); err != nil {
			logoutErr := tc.Logout()
			return trace.NewAggregate(err, logoutErr)
		}
	}

	// Update the command line flag for the proxy to make sure any advertised
	// settings are picked up.
	webProxyHost, _ := tc.WebProxyHostPort()
	cf.Proxy = webProxyHost

	profile, profiles, err = cf.FullProfileStatus()
	if err != nil {
		return trace.Wrap(err)
	}

	// Print status to show information of the logged in user.
	if err := printLoginInformation(cf, profile, profiles, cf.getAccessListsToReview(tc)); err != nil {
		return trace.Wrap(err)
	}

	// NOTE: we currently print all alerts that are marked as `on-login`, because we
	// don't use the alert API very heavily. If we start to make more use of it, we
	// could probably add a separate `tsh alerts ls` command, and truncate the list
	// with a message like "run 'tsh alerts ls' to view N additional alerts".
	if err := common.ShowClusterAlerts(cf.Context, clusterClient.CurrentCluster(), os.Stderr, map[string]string{
		types.AlertOnLogin: "yes",
	}, types.AlertSeverity_LOW); err != nil {
		logger.WarnContext(cf.Context, "Failed to display cluster alerts", "error", err)
	}

	return nil
}

// onLogout deletes a "session certificate" from ~/.tsh for a given proxy
func onLogout(cf *CLIConf) error {
	// Extract all clusters the user is currently logged into.
	active, available, err := cf.FullProfileStatus()
	if err != nil && !trace.IsCompareFailed(err) {
		if trace.IsNotFound(err) {
			fmt.Printf("All users logged out.\n")
			return nil
		} else if trace.IsAccessDenied(err) {
			fmt.Printf("%v: Logged in user does not have the correct permissions\n", err)
			return nil
		}
		return trace.Wrap(err)
	}
	profiles := slices.Clone(available)
	if active != nil {
		profiles = append(profiles, active)
	}

	// Extract the proxy name.
	proxyHost, _, err := net.SplitHostPort(cf.Proxy)
	if err != nil {
		proxyHost = cf.Proxy
	}

	switch {
	// Proxy and username for key to remove.
	case proxyHost != "" && cf.Username != "":
		tc, err := makeClient(cf)
		if err != nil {
			return trace.Wrap(err)
		}
		// Load profile for the requested proxy/user.
		profile, err := tc.ProfileStatus()
		if err != nil && !trace.IsNotFound(err) && !trace.IsCompareFailed(err) {
			return trace.Wrap(err)
		}

		// Log out user from the databases.
		if profile != nil {
			for _, db := range profile.Databases {
				logger.DebugContext(cf.Context, "Logging user out of database",
					"user", profile.Name,
					"database", db,
				)
				err = dbprofile.Delete(tc, db)
				if err != nil {
					return trace.Wrap(err)
				}
			}
		}

		// Remove keys for this user from disk and running agent.
		err = tc.Logout()
		if err != nil {
			if trace.IsNotFound(err) {
				fmt.Printf("User %v already logged out from %v.\n", cf.Username, proxyHost)
				return trace.Wrap(&common.ExitCodeError{Code: 1})
			}
			return trace.Wrap(err)
		}

		// Remove Teleport related entries from kubeconfig.
		logger.DebugContext(cf.Context, "Removing Teleport related entries from kubeconfig", "cluster_addr", tc.KubeClusterAddr())
		err = kubeconfig.RemoveByServerAddr("", tc.KubeClusterAddr())
		if err != nil {
			return trace.Wrap(err)
		}

		fmt.Printf("Logged out %v from %v.\n", cf.Username, proxyHost)
	// Remove all keys.
	case proxyHost == "" && cf.Username == "":
		tc, err := makeClient(cf)
		if err != nil {
			return trace.Wrap(err)
		}
		logger.DebugContext(cf.Context, "Removing Teleport related entries from kubeconfig", "cluster_addr", tc.KubeClusterAddr())
		if err = kubeconfig.RemoveByServerAddr("", tc.KubeClusterAddr()); err != nil {
			return trace.Wrap(err)
		}

		// Remove Teleport related entries from kubeconfig for all clusters.
		for _, profile := range profiles {
			logger.DebugContext(cf.Context, "Removing Teleport related entries from kubeconfig", "cluster", profile.Cluster)
			err = kubeconfig.RemoveByClusterName("", profile.Cluster)
			if err != nil {
				return trace.Wrap(err)
			}
		}

		// Remove all database access related profiles as well such as Postgres
		// connection service file.
		for _, profile := range profiles {
			for _, db := range profile.Databases {
				logger.DebugContext(cf.Context, "Logging user out of database",
					"user", profile.Name,
					"database", db,
				)
				err = dbprofile.Delete(tc, db)
				if err != nil {
					return trace.Wrap(err)
				}
			}
		}

		err = forEachProfileParallel(cf, func(ctx context.Context, tc *client.TeleportClient, profile *client.ProfileStatus) error {
			if !profile.SAMLSingleLogoutEnabled {
				return nil
			}
			clt, err := tc.ConnectToCluster(ctx)
			if err != nil {
				return trace.Wrap(err)
			}
			defer clt.Close()
			sloURL, err := tc.GetSAMLSingleLogoutURL(ctx, clt, profile)
			if err != nil {
				return trace.WrapWithMessage(err, "failed to retrieve SAML single logout URL.")
			}
			if sloURL == "" {
				return trace.WrapWithMessage(err, "SAML single logout is enabled, but no single logout URL is available.")
			}
			return trace.Wrap(tc.SAMLSingleLogout(ctx, sloURL))
		})
		if err != nil {
			fmt.Printf("We were unable to log you out of your SAML identity provider: %v", err)
		}

		// Remove all keys from disk and the running agent.
		err = tc.LogoutAll()
		if err != nil {
			return trace.Wrap(err)
		}

		fmt.Printf("Logged out all users from all proxies.\n")
	case proxyHost != "" && cf.Username == "":
		fmt.Printf("Specify --user to log out a specific user from %q or remove the --proxy flag to log out all users from all proxies.\n", proxyHost)
	case proxyHost == "" && cf.Username != "":
		fmt.Printf("Specify --proxy to log out user %q from a specific proxy or remove the --user flag to log out all users from all proxies.\n", cf.Username)
	}
	return nil
}

// onListNodes executes 'tsh ls' command.
func onListNodes(cf *CLIConf) error {
	if cf.ListAll {
		return trace.Wrap(listNodesAllClusters(cf))
	}

	tc, err := makeClient(cf)
	if err != nil {
		return trace.Wrap(err)
	}

	tc.AllowHeadless = true

	// Get list of all nodes in backend and sort by "Node Name".
	var nodes []types.Server
	err = client.RetryWithRelogin(cf.Context, tc, func() error {
		nodes, err = tc.ListNodesWithFilters(cf.Context)
		return err
	})
	if err != nil {
		return trace.Wrap(err)
	}
	sort.Slice(nodes, func(i, j int) bool {
		return nodes[i].GetHostname() < nodes[j].GetHostname()
	})

	if err := printNodes(nodes, cf); err != nil {
		return trace.Wrap(err)
	}

	return nil
}

// clusterClient is a client for a particular cluster
type clusterClient struct {
	name            string
	connectionError error
	cluster         *client.ClusterClient
	auth            authclient.ClientI
	profile         *client.ProfileStatus
	req             proto.ListResourcesRequest
}

func (c *clusterClient) Close() error {
	if c.connectionError != nil {
		return nil
	}

	return trace.NewAggregate(c.auth.Close(), c.cluster.Close())
}

// getClusterClients establishes a ProxyClient to every cluster
// that the user has valid credentials for
func getClusterClients(cf *CLIConf, resource string) ([]*clusterClient, error) {
	tracer := cf.TracingProvider.Tracer(teleport.ComponentTSH)

	// mu guards access to clusters
	var (
		mu       sync.Mutex
		clusters []*clusterClient
	)

	err := forEachProfileParallel(cf, func(ctx context.Context, tc *client.TeleportClient, profile *client.ProfileStatus) error {
		ctx, span := tracer.Start(
			ctx,
			"getClusterClient",
			oteltrace.WithAttributes(attribute.String("cluster", profile.Cluster)),
		)
		defer span.End()

		logger := logger.With("cluster", profile.Cluster)

		logger.DebugContext(ctx, "Creating client")
		clt, err := tc.ConnectToCluster(ctx)
		if err != nil {
			// log error and return nil so that results may still be retrieved
			// for other clusters.
			logger.ErrorContext(ctx, "Failed connecting to proxy", "error", err)

			mu.Lock()
			clusters = append(clusters, &clusterClient{
				name:            profile.Cluster,
				connectionError: trace.ConnectionProblem(err, "failed to connect to cluster %s: %v", profile.Cluster, err),
			})
			mu.Unlock()
			return nil
		}

		// Add the local cluster to the output
		mu.Lock()
		clusters = append(clusters, &clusterClient{
			name:    tc.SiteName,
			cluster: clt,
			auth:    clt.AuthClient,
			profile: profile,
			req:     *tc.ResourceFilter(resource),
		})
		mu.Unlock()

		// Check if the user has access to any attached remote clusters.
		remoteClusters, err := clt.AuthClient.GetRemoteClusters(ctx)
		if err != nil {
			// Log that an error happened but do not return an error to
			// prevent results from other clusters from being retrieved.
			logger.ErrorContext(ctx, "Failed to lookup leaf clusters", "error", err)
			return nil
		}

		localClusters := make([]*clusterClient, 0, len(remoteClusters))
		for _, cluster := range remoteClusters {
			clusterName := cluster.GetName()
			auth, err := clt.ConnectToCluster(ctx, clusterName)
			if err != nil {
				localClusters = append(localClusters, &clusterClient{
					name:            clusterName,
					connectionError: trace.ConnectionProblem(err, "failed to connect to cluster %s: %v", clusterName, err),
				})
				continue
			}

			localClusters = append(localClusters, &clusterClient{
				cluster: clt,
				auth:    auth,
				profile: profile,
				name:    clusterName,
				req:     *tc.ResourceFilter(resource),
			})
		}

		mu.Lock()
		clusters = append(clusters, localClusters...)
		mu.Unlock()

		return nil
	})

	return clusters, trace.Wrap(err)
}

type nodeListing struct {
	Proxy   string       `json:"proxy"`
	Cluster string       `json:"cluster"`
	Node    types.Server `json:"node"`
}

type nodeListings []nodeListing

func (l nodeListings) Len() int {
	return len(l)
}

func (l nodeListings) Less(i, j int) bool {
	if l[i].Proxy != l[j].Proxy {
		return l[i].Proxy < l[j].Proxy
	}
	if l[i].Cluster != l[j].Cluster {
		return l[i].Cluster < l[j].Cluster
	}
	return l[i].Node.GetHostname() < l[j].Node.GetHostname()
}

func (l nodeListings) Swap(i, j int) {
	l[i], l[j] = l[j], l[i]
}

func listNodesAllClusters(cf *CLIConf) error {
	tracer := cf.TracingProvider.Tracer(teleport.ComponentTSH)
	clusters, err := getClusterClients(cf, types.KindNode)
	if err != nil {
		return trace.Wrap(err)
	}

	defer func() {
		// close all clients
		for _, cluster := range clusters {
			_ = cluster.Close()
		}
	}()

	// Fetch node listings for all clusters in parallel with an upper limit
	group, groupCtx := errgroup.WithContext(cf.Context)
	group.SetLimit(10)

	var (
		mu       sync.Mutex
		listings nodeListings
		errors   []error
	)

	for _, cluster := range clusters {
		if cluster.connectionError != nil {
			mu.Lock()
			errors = append(errors, cluster.connectionError)
			mu.Unlock()
			continue
		}

		group.Go(func() error {
			ctx, span := tracer.Start(
				groupCtx,
				"ListNodes",
				oteltrace.WithAttributes(attribute.String("cluster", cluster.name)))
			defer span.End()

			logger := logger.With("cluster", cluster.name)
			nodes, err := apiclient.GetAllResources[types.Server](ctx, cluster.auth, &cluster.req)
			if err != nil {
				logger.ErrorContext(ctx, "Failed to get nodes", "error", err)

				mu.Lock()
				errors = append(errors, trace.ConnectionProblem(err, "failed to list nodes for cluster %s: %v", cluster.name, err))
				mu.Unlock()
				return nil
			}

			localListings := make(nodeListings, 0, len(nodes))
			for _, node := range nodes {
				localListings = append(localListings, nodeListing{
					Proxy:   cluster.profile.ProxyURL.Host,
					Cluster: cluster.name,
					Node:    node,
				})
			}
			mu.Lock()
			listings = append(listings, localListings...)
			mu.Unlock()

			return nil
		})
	}

	// wait for all nodes to be retrieved
	if err := group.Wait(); err != nil {
		return trace.Wrap(err)
	}

	if len(listings) == 0 && len(errors) > 0 {
		return trace.NewAggregate(errors...)
	}

	sort.Sort(listings)

	format := strings.ToLower(cf.Format)
	switch format {
	case teleport.Text, "":
		if err := printNodesWithClusters(listings, cf.Verbose, cf.Stdout()); err != nil {
			return trace.Wrap(err)
		}
	case teleport.JSON, teleport.YAML:
		out, err := serializeNodesWithClusters(listings, format)
		if err != nil {
			return trace.Wrap(err)
		}

		if _, err := fmt.Fprintln(cf.Stdout(), out); err != nil {
			return trace.Wrap(err)
		}
	default:
		return trace.BadParameter("unsupported format %q", format)
	}

	// Sometimes a user won't see any nodes because they're missing principals.
	if len(listings) == 0 {
		if _, err := fmt.Fprintln(cf.Stderr(), emptyNodesFooter); err != nil {
			return trace.Wrap(err)
		}
	}

	return trace.NewAggregate(errors...)
}

func printNodesWithClusters(nodes []nodeListing, verbose bool, output io.Writer) error {
	var rows [][]string
	for _, n := range nodes {
		rows = append(rows, getNodeRow(n.Proxy, n.Cluster, n.Node, verbose))
	}
	var t asciitable.Table
	if verbose {
		t = asciitable.MakeTable([]string{"Proxy", "Cluster", "Node Name", "Node ID", "Address", "Labels"}, rows...)
	} else {
		t = asciitable.MakeTableWithTruncatedColumn([]string{"Proxy", "Cluster", "Node Name", "Address", "Labels"}, rows, "Labels")
	}
	if _, err := fmt.Fprintln(output, t.AsBuffer().String()); err != nil {
		return trace.Wrap(err)
	}
	return nil
}

func serializeNodesWithClusters(nodes []nodeListing, format string) (string, error) {
	var out []byte
	var err error
	if format == teleport.JSON {
		out, err = utils.FastMarshalIndent(nodes, "", "  ")
	} else {
		out, err = yaml.Marshal(nodes)
	}
	return string(out), trace.Wrap(err)
}

func getAccessRequest(ctx context.Context, tc *client.TeleportClient, requestID, username string) (types.AccessRequest, error) {
	var req types.AccessRequest
	err := tc.WithRootClusterClient(ctx, func(clt authclient.ClientI) error {
		reqs, err := clt.GetAccessRequests(ctx, types.AccessRequestFilter{
			ID:   requestID,
			User: username,
		})
		if err != nil {
			return trace.Wrap(err)
		}
		if len(reqs) != 1 {
			return trace.BadParameter(`invalid access request "%v"`, requestID)
		}
		req = reqs[0]
		return nil
	})
	return req, trace.Wrap(err)
}

func createAccessRequest(cf *CLIConf) (types.AccessRequest, error) {
	roles := utils.SplitIdentifiers(cf.DesiredRoles)
	reviewers := utils.SplitIdentifiers(cf.SuggestedReviewers)
	requestedResourceIDs, err := types.ResourceIDsFromStrings(cf.RequestedResourceIDs)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	req, err := services.NewAccessRequestWithResources(cf.Username, roles, requestedResourceIDs)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	req.SetRequestReason(cf.RequestReason)
	req.SetSuggestedReviewers(reviewers)

	// Only set RequestTTL and SessionTTL if values are greater than zero.
	// Otherwise, leave defaults, and the server will take the zero values and
	// transform them into default expirations accordingly.
	if cf.RequestTTL > 0 {
		req.SetExpiry(time.Now().UTC().Add(cf.RequestTTL))
	}
	if cf.SessionTTL > 0 {
		req.SetAccessExpiry(time.Now().UTC().Add(cf.SessionTTL))
	}
	if cf.MaxDuration > 0 {
		// Time will be relative to the approval time instead of the request time.
		req.SetMaxDuration(time.Now().UTC().Add(cf.MaxDuration))
	}

	if cf.AssumeStartTimeRaw != "" {
		assumeStartTime, err := time.Parse(time.RFC3339, cf.AssumeStartTimeRaw)
		if err != nil {
			return nil, trace.BadParameter("parsing assume-start-time (required format RFC3339 e.g 2023-12-12T23:20:50.52Z): %v", err)
		}

		req.SetAssumeStartTime(assumeStartTime)
	}

	return req, nil
}

func executeAccessRequest(cf *CLIConf, tc *client.TeleportClient) error {
	if cf.DesiredRoles == "" && cf.RequestID == "" && len(cf.RequestedResourceIDs) == 0 {
		return trace.BadParameter("at least one role or resource or a request ID must be specified")
	}
	if cf.RequestTTL < 0 {
		return trace.BadParameter("request TTL value must be greater than zero")
	}
	if cf.SessionTTL < 0 {
		return trace.BadParameter("session TTL value must be greater than zero")
	}
	if cf.Username == "" {
		cf.Username = tc.Username
	}

	var req types.AccessRequest
	var err error
	if cf.RequestID != "" {
		// This access request already exists, fetch it.
		req, err = getAccessRequest(cf.Context, tc, cf.RequestID, cf.Username)
		if err != nil {
			return trace.Wrap(err)
		}
		// If the request isn't pending, handle resolution
		if !req.GetState().IsPending() {
			err := onRequestResolution(cf, tc, req)
			return trace.Wrap(err)
		}
		fmt.Fprint(os.Stdout, "Request pending...\n")
	} else {
		// This is a new access request, create it. This just creates the local
		// object, it is not yet sent to the backend.
		req, err = createAccessRequest(cf)
		if err != nil {
			return trace.Wrap(err)
		}
	}

	// Upsert request if it doesn't already exist.
	if cf.RequestID == "" {
		fmt.Fprint(os.Stdout, "Creating request...\n")
		// always create access request against the root cluster
		if err := tc.WithRootClusterClient(cf.Context, func(clt authclient.ClientI) error {
			req, err = clt.CreateAccessRequestV2(cf.Context, req)
			return trace.Wrap(err)
		}); err != nil {
			if strings.Contains(err.Error(), services.InvalidKubernetesKindAccessRequest) {
				return trace.BadParameter("%s\nTry searching for specific kinds with:\n> tsh request search --kube-cluster=KUBE_CLUSTER_NAME --kind=KIND", err.Error())
			}
			return trace.Wrap(err)
		}
		cf.RequestID = req.GetName()
	}

	onRequestShow(cf)
	fmt.Println("")

	// Don't wait for request to get resolved, just print out request info.
	if cf.NoWait {
		return nil
	}

	// Wait for the request to be resolved.
	fmt.Fprintf(os.Stdout, "Waiting for request approval...\n")

	var resolvedReq types.AccessRequest
	if err := tc.WithRootClusterClient(cf.Context, func(clt authclient.ClientI) error {
		resolvedReq, err = awaitRequestResolution(cf.Context, clt, req)
		return trace.Wrap(err)
	}); err != nil {
		return trace.Wrap(err)
	}

	// Handle resolution and update client certs if approved.
	return trace.Wrap(onRequestResolution(cf, tc, resolvedReq))
}

func printNodes(nodes []types.Server, conf *CLIConf) error {
	format := strings.ToLower(conf.Format)
	switch format {
	case teleport.Text, "":
		if err := printNodesAsText(conf.Stdout(), nodes, conf.Verbose); err != nil {
			return trace.Wrap(err)
		}
	case teleport.JSON, teleport.YAML:
		out, err := serializeNodes(nodes, format)
		if err != nil {
			return trace.Wrap(err)
		}
		if _, err := fmt.Fprintln(conf.Stdout(), out); err != nil {
			return trace.Wrap(err)
		}
	case teleport.Names:
		for _, n := range nodes {
			if _, err := fmt.Fprintln(conf.Stdout(), n.GetHostname()); err != nil {
				return trace.Wrap(err)
			}
		}
	default:
		return trace.BadParameter("unsupported format %q", format)
	}

	// Sometimes a user won't see any nodes because they're missing principals.
	if len(nodes) == 0 {
		if _, err := fmt.Fprintln(conf.Stderr(), emptyNodesFooter); err != nil {
			return trace.Wrap(err)
		}
	}

	return nil
}

func serializeNodes(nodes []types.Server, format string) (string, error) {
	if nodes == nil {
		nodes = []types.Server{}
	}
	var out []byte
	var err error
	if format == teleport.JSON {
		out, err = utils.FastMarshalIndent(nodes, "", "  ")
	} else {
		out, err = yaml.Marshal(nodes)
	}
	return string(out), trace.Wrap(err)
}

func getNodeRow(proxy, cluster string, node types.Server, verbose bool) []string {
	// Reusable function to get addr or tunnel for each node
	getAddr := func(n types.Server) string {
		if n.GetUseTunnel() {
			return "⟵ Tunnel"
		}
		return n.GetAddr()
	}

	row := make([]string, 0)
	if proxy != "" && cluster != "" {
		row = append(row, proxy, cluster)
	}

	labels := common.FormatLabels(node.GetAllLabels(), verbose)
	if verbose {
		row = append(row, node.GetHostname(), node.GetName(), getAddr(node), labels)
	} else {
		row = append(row, node.GetHostname(), getAddr(node), labels)
	}
	return row
}

func printNodesAsText[T types.Server](output io.Writer, nodes []T, verbose bool) error {
	var rows [][]string
	for _, n := range nodes {
		rows = append(rows, getNodeRow("", "", n, verbose))
	}
	var t asciitable.Table
	switch verbose {
	// In verbose mode, print everything on a single line and include the Node
	// ID (UUID). Useful for machines that need to parse the output of "tsh ls".
	case true:
		t = asciitable.MakeTable([]string{"Node Name", "Node ID", "Address", "Labels"}, rows...)
	// In normal mode chunk the labels and print two per line and allow multiple
	// lines per node.
	case false:
		t = asciitable.MakeTableWithTruncatedColumn([]string{"Node Name", "Address", "Labels"}, rows, "Labels")
	}
	if _, err := fmt.Fprintln(output, t.AsBuffer().String()); err != nil {
		return trace.Wrap(err)
	}

	return nil
}

func showApps(apps []types.Application, active []tlsca.RouteToApp, w io.Writer, format string, verbose bool) error {
	format = strings.ToLower(format)
	switch format {
	case teleport.Text, "":
		appListings := make([]appListing, 0, len(apps))
		for _, app := range apps {
			appListings = append(appListings, appListing{App: app})
		}

		if err := writeAppTable(w, appListings, appTableConfig{
			listAll: false, // showApps lists apps from a single cluster.
			active:  active,
			verbose: verbose,
		}); err != nil {
			return trace.Wrap(err)
		}
	case teleport.JSON, teleport.YAML:
		out, err := serializeApps(apps, format)
		if err != nil {
			return trace.Wrap(err)
		}
		if _, err := fmt.Fprintln(w, out); err != nil {
			return trace.Wrap(err)
		}
	default:
		return trace.BadParameter("unsupported format %q", format)
	}
	return nil
}

func serializeApps(apps []types.Application, format string) (string, error) {
	if apps == nil {
		apps = []types.Application{}
	}
	var out []byte
	var err error
	if format == teleport.JSON {
		out, err = utils.FastMarshalIndent(apps, "", "  ")
	} else {
		out, err = yaml.Marshal(apps)
	}
	return string(out), trace.Wrap(err)
}

type appTableConfig struct {
	// active is a list of apps for which the user retrieved a short-lived cert with tsh app login.
	active []tlsca.RouteToApp
	// verbose makes the table show extra columns.
	verbose bool
	// listAll makes the table render two extra columns: Proxy and Cluster.
	listAll bool
}

func writeAppTable(w io.Writer, appListings []appListing, config appTableConfig) error {
	includesMultiPortApp := slices.ContainsFunc(appListings, func(al appListing) bool {
		return len(al.App.GetTCPPorts()) > 0
	})

	getName := func(app types.Application) string {
		isActive := slices.ContainsFunc(config.active, func(route tlsca.RouteToApp) bool {
			// TODO(ravicious): This should be based on name _and_ route.ClusterName, so that we don't
			// incorrectly show multiple apps with the same name but from different clusters as active.
			// However, to do this we'd need to double check if route.ClusterName always matches
			// appListing.Cluster (and also fill out that field in showApps).
			return route.Name == app.GetName()
		})

		if isActive {
			return fmt.Sprintf("> %s", app.GetName())
		}

		return app.GetName()
	}
	getLabels := func(app types.Application) string {
		return common.FormatLabels(app.GetAllLabels(), config.verbose)
	}
	getTargetPorts := func(app types.Application) string {
		return app.GetTCPPorts().String()
	}

	const labelsColumn = "Labels"
	allColumns := []appTableColumn{
		appTableColumn{
			name:           "Proxy",
			getFromListing: appListing.GetProxy,
			hide:           !config.listAll,
		},
		appTableColumn{
			name:           "Cluster",
			getFromListing: appListing.GetCluster,
			hide:           !config.listAll,
		},
		appTableColumn{
			name: "Application",
			get:  getName,
		},
		appTableColumn{
			name: "Description",
			get:  types.Application.GetDescription,
		},
		appTableColumn{
			name: "Type",
			get:  types.Application.GetProtocol,
		},
		appTableColumn{
			name: "Public Address",
			get:  types.Application.GetPublicAddr,
		},
		appTableColumn{
			name: "Target Ports",
			get:  getTargetPorts,
			hide: !includesMultiPortApp,
		},
		appTableColumn{
			name: "URI",
			get:  types.Application.GetURI,
			hide: !config.verbose,
		},
		appTableColumn{
			name: labelsColumn,
			get:  getLabels,
		},
	}
	columns := slices.DeleteFunc(allColumns, func(column appTableColumn) bool { return column.hide })

	headers := make([]string, 0, len(columns))
	for _, column := range columns {
		headers = append(headers, column.name)
	}

	rows := make([][]string, 0, len(appListings))
	for _, appListing := range appListings {
		appRow := make([]string, 0, len(columns))

		for _, column := range columns {
			var content string
			switch {
			case column.get != nil:
				content = column.get(appListing.App)
			case column.getFromListing != nil:
				content = column.getFromListing(appListing)
			}

			appRow = append(appRow, content)
		}

		rows = append(rows, appRow)
	}

	// In verbose mode, print everything on a single line.
	// In normal mode, chunk the labels, print two per line and allow multiple
	// lines per app.
	var t asciitable.Table
	if config.verbose {
		t = asciitable.MakeTable(headers, rows...)
	} else {
		t = asciitable.MakeTableWithTruncatedColumn(headers, rows, labelsColumn)
	}

	_, err := fmt.Fprintln(w, t.AsBuffer().String())
	return trace.Wrap(err)
}

type appTableColumn struct {
	name           string
	get            func(app types.Application) string
	getFromListing func(listing appListing) string
	hide           bool
}

func showDatabases(cf *CLIConf, databases []types.Database, active []tlsca.RouteToDatabase, accessChecker services.AccessChecker) error {
	format := strings.ToLower(cf.Format)
	switch format {
	case teleport.Text, "":
		showDatabasesAsText(cf, cf.Stdout(), databases, active, accessChecker, cf.Verbose)
	case teleport.JSON, teleport.YAML:
		out, err := serializeDatabases(databases, cf.Format, accessChecker)
		if err != nil {
			return trace.Wrap(err)
		}
		fmt.Fprintln(cf.Stdout(), out)
	default:
		return trace.BadParameter("unsupported format %q", format)
	}
	return nil
}

func serializeDatabases(databases []types.Database, format string, accessChecker services.AccessChecker) (string, error) {
	if databases == nil {
		databases = []types.Database{}
	}

	printObj, err := getDatabasePrintObject(databases, accessChecker)
	if err != nil {
		return "", trace.Wrap(err)
	}

	var out []byte
	switch format {
	case teleport.JSON:
		out, err = utils.FastMarshalIndent(printObj, "", "  ")
	default:
		out, err = yaml.Marshal(printObj)
	}
	return string(out), trace.Wrap(err)
}

func getDatabasePrintObject(databases []types.Database, accessChecker services.AccessChecker) (any, error) {
	if accessChecker == nil || len(accessChecker.RoleNames()) == 0 || len(databases) == 0 {
		return databases, nil
	}
	dbsWithUsers, err := getDatabasesWithUsers(databases, accessChecker)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return dbsWithUsers, nil
}

type dbUsers struct {
	Allowed []string `json:"allowed,omitempty"`
	Denied  []string `json:"denied,omitempty"`
}

type databaseWithUsers struct {
	// *DatabaseV3 is used instead of types.Database because we want the db fields marshaled to JSON inline.
	// An embedded interface (like types.Database) does not inline when marshaled to JSON.
	*types.DatabaseV3
	Users         *dbUsers `json:"users"`
	DatabaseRoles []string `json:"database_roles,omitempty"`
}

func getDBUsers(db types.Database, accessChecker services.AccessChecker) *dbUsers {
	users, err := accessChecker.EnumerateDatabaseUsers(db)
	if err != nil {
		logger.WarnContext(context.Background(), "Failed to EnumerateDatabaseUsers for database",
			"database", db.GetName(),
			"error", err,
		)
		return &dbUsers{}
	}
	allowed, denied := users.ToEntities()
	return &dbUsers{
		Allowed: allowed,
		Denied:  denied,
	}
}

func newDatabaseWithUsers(db types.Database, accessChecker services.AccessChecker) (*databaseWithUsers, error) {
	dbWithUsers := &databaseWithUsers{
		Users: getDBUsers(db, accessChecker),
	}
	switch db := db.(type) {
	case *types.DatabaseV3:
		dbWithUsers.DatabaseV3 = db
	default:
		return nil, trace.BadParameter("unrecognized database type %T", db)
	}

	if db.IsAutoUsersEnabled() {
		roles, err := accessChecker.CheckDatabaseRoles(db, nil)
		if err != nil {
			logger.WarnContext(context.Background(), "Failed to CheckDatabaseRoles for database",
				"database", db.GetName(),
				"error", err,
			)
		} else {
			dbWithUsers.DatabaseRoles = roles
		}
	}
	return dbWithUsers, nil
}

func getDatabasesWithUsers(databases types.Databases, accessChecker services.AccessChecker) ([]*databaseWithUsers, error) {
	var dbsWithUsers []*databaseWithUsers
	for _, db := range databases {
		dbWithUsers, err := newDatabaseWithUsers(db, accessChecker)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		dbsWithUsers = append(dbsWithUsers, dbWithUsers)
	}
	return dbsWithUsers, nil
}

func serializeDatabasesAllClusters(dbListings []databaseListing, format string) (string, error) {
	if dbListings == nil {
		dbListings = []databaseListing{}
	}
	var out []byte
	var err error
	if format == teleport.JSON {
		out, err = utils.FastMarshalIndent(dbListings, "", "  ")
	} else {
		out, err = yaml.Marshal(dbListings)
	}
	return string(out), trace.Wrap(err)
}

func formatUsersForDB(database types.Database, accessChecker services.AccessChecker) (users string) {
	// may happen if fetching the role set failed for any reason.
	if accessChecker == nil {
		return "(unknown)"
	}

	dbUsers := getDBUsers(database, accessChecker)

	// Add a note for auto-provisioned user.
	if database.IsAutoUsersEnabled() {
		autoUser, err := accessChecker.DatabaseAutoUserMode(database)
		if err != nil {
			logger.WarnContext(context.Background(), "Failed to get DatabaseAutoUserMode for database",
				"database", database.GetName(),
				"error", err,
			)
		} else if autoUser.IsEnabled() {
			defer func() {
				users = users + " (Auto-provisioned)"
			}()
		}
	}

	return common.FormatAllowedEntities(dbUsers.Allowed, dbUsers.Denied)
}

// TODO(greedy52) more refactoring on db printing and move them to db_print.go.

func getDatabaseRow(proxy, cluster, clusterFlag string, database types.Database, active []tlsca.RouteToDatabase, accessChecker services.AccessChecker, verbose bool) databaseTableRow {
	displayName := common.FormatResourceName(database, verbose)
	var connect string
	for _, a := range active {
		if a.ServiceName == database.GetName() {
			// format the db name with the display name
			displayName = formatActiveDB(a, displayName)
			// then revert it for connect string
			switch a.Protocol {
			case defaults.ProtocolDynamoDB:
				// DynamoDB does not support "tsh db connect", so print the proxy command instead.
				connect = formatDatabaseProxyCommand(clusterFlag, a)
			default:
				connect = formatDatabaseConnectCommand(clusterFlag, a)
			}
			break
		}
	}

	return databaseTableRow{
		Proxy:         proxy,
		Cluster:       cluster,
		DisplayName:   displayName,
		Description:   database.GetDescription(),
		Protocol:      database.GetProtocol(),
		Type:          database.GetType(),
		URI:           database.GetURI(),
		AllowedUsers:  formatUsersForDB(database, accessChecker),
		DatabaseRoles: formatDatabaseRolesForDB(database, accessChecker),
		Labels:        common.FormatLabels(database.GetAllLabels(), verbose),
		Connect:       connect,
	}
}

func showDatabasesAsText(cf *CLIConf, w io.Writer, databases []types.Database, active []tlsca.RouteToDatabase, accessChecker services.AccessChecker, verbose bool) {
	var rows []databaseTableRow
	for _, database := range databases {
		rows = append(rows, getDatabaseRow("", "",
			cf.SiteName,
			database,
			active,
			accessChecker,
			verbose))
	}
	printDatabaseTable(printDatabaseTableConfig{
		writer:  w,
		rows:    rows,
		verbose: verbose,
	})

	maybeShowListDatabasesHint(cf, w, len(rows))
}

func printDatabasesWithClusters(cf *CLIConf, dbListings []databaseListing, active []tlsca.RouteToDatabase) {
	var rows []databaseTableRow
	for _, listing := range dbListings {
		rows = append(rows, getDatabaseRow(
			listing.Proxy,
			listing.Cluster,
			cf.SiteName,
			listing.Database,
			active,
			listing.accessChecker,
			cf.Verbose))
	}
	printDatabaseTable(printDatabaseTableConfig{
		writer:              cf.Stdout(),
		rows:                rows,
		showProxyAndCluster: true,
		verbose:             cf.Verbose,
	})

	maybeShowListDatabasesHint(cf, cf.Stdout(), len(rows))
}

func formatActiveDB(active tlsca.RouteToDatabase, displayName string) string {
	active.ServiceName = displayName

	var details []string
	if active.Username != "" {
		details = append(details, fmt.Sprintf("user: %s", active.Username))
	}
	if active.Database != "" {
		details = append(details, fmt.Sprintf("db: %s", active.Database))
	}
	if len(active.Roles) > 0 {
		details = append(details, fmt.Sprintf("roles: %v", active.Roles))
	}

	if len(details) == 0 {
		return fmt.Sprintf("> %v", active.ServiceName)
	}
	return fmt.Sprintf("> %v (%v)", active.ServiceName, strings.Join(details, ", "))
}

// onListClusters executes 'tsh clusters' command
func onListClusters(cf *CLIConf) error {
	tc, err := makeClient(cf)
	if err != nil {
		return trace.Wrap(err)
	}

	var rootClusterName string
	var leafClusters []types.RemoteCluster
	err = client.RetryWithRelogin(cf.Context, tc, func() error {
		clusterClient, err := tc.ConnectToCluster(cf.Context)
		if err != nil {
			return err
		}
		defer clusterClient.Close()

		rootClusterName = clusterClient.RootClusterName()

		rootAuthClient, err := clusterClient.ConnectToRootCluster(cf.Context)
		if err != nil {
			return trace.Wrap(err)
		}
		defer rootAuthClient.Close()

		leafClusters, err = rootAuthClient.GetRemoteClusters(cf.Context)
		return trace.Wrap(err)
	})
	if err != nil {
		return trace.Wrap(err)
	}

	profile, err := cf.ProfileStatus()
	if err != nil {
		return trace.Wrap(err)
	}
	isSelected := func(clusterName string) bool {
		return profile != nil && clusterName == profile.Cluster
	}
	showSelected := func(clusterName string) string {
		if isSelected(clusterName) {
			return "*"
		}
		return ""
	}

	format := strings.ToLower(cf.Format)
	switch format {
	case teleport.Text, "":
		header := []string{"Cluster Name", "Status", "Cluster Type", "Labels", "Selected"}
		rows := [][]string{
			{rootClusterName, teleport.RemoteClusterStatusOnline, "root", "", showSelected(rootClusterName)},
		}
		for _, cluster := range leafClusters {
			labels := common.FormatLabels(cluster.GetAllLabels(), cf.Verbose)
			rows = append(rows, []string{
				cluster.GetName(), cluster.GetConnectionStatus(), "leaf", labels, showSelected(cluster.GetName()),
			})
		}

		var t asciitable.Table
		switch {
		case cf.Quiet:
			t = asciitable.MakeHeadlessTable(4)
			for _, row := range rows {
				t.AddRow(row)
			}
		case cf.Verbose:
			t = asciitable.MakeTable(header, rows...)
		default:
			t = asciitable.MakeTableWithTruncatedColumn(header, rows, "Labels")
		}

		fmt.Println(t.AsBuffer().String())
	case teleport.JSON, teleport.YAML:
		rootClusterInfo := clusterInfo{
			ClusterName: rootClusterName,
			Status:      teleport.RemoteClusterStatusOnline,
			ClusterType: "root",
			Selected:    isSelected(rootClusterName),
		}
		leafClusterInfo := make([]clusterInfo, 0, len(leafClusters))
		for _, leaf := range leafClusters {
			leafClusterInfo = append(leafClusterInfo, clusterInfo{
				ClusterName: leaf.GetName(),
				Status:      leaf.GetConnectionStatus(),
				ClusterType: "leaf",
				Labels:      leaf.GetAllLabels(),
				Selected:    isSelected(leaf.GetName()),
			})
		}
		out, err := serializeClusters(rootClusterInfo, leafClusterInfo, format)
		if err != nil {
			return trace.Wrap(err)
		}
		fmt.Println(out)
	default:
		return trace.BadParameter("unsupported format %q", cf.Format)
	}
	return nil
}

func onListSessions(cf *CLIConf) error {
	tc, err := makeClient(cf)
	if err != nil {
		return trace.Wrap(err)
	}

	clt, err := tc.ConnectToCluster(cf.Context)
	if err != nil {
		return trace.Wrap(err)
	}
	defer clt.Close()

	sessions, err := clt.AuthClient.GetActiveSessionTrackers(cf.Context)
	if err != nil {
		return trace.Wrap(err)
	}

	kinds := map[string]types.SessionKind{
		"ssh":     types.SSHSessionKind,
		"db":      types.DatabaseSessionKind,
		"app":     types.AppSessionKind,
		"desktop": types.WindowsDesktopSessionKind,
		"k8s":     types.KubernetesSessionKind,
		// tsh commands often use "kube" to mean kubernetes,
		// so add an alias to make it more intuitive
		"kube": types.KubernetesSessionKind,
	}

	var filter []types.SessionKind
	for _, k := range cf.SessionKinds {
		filter = append(filter, kinds[k])
	}
	sessions = sortAndFilterSessions(sessions, filter)
	return trace.Wrap(serializeSessions(sessions, strings.ToLower(cf.Format), cf.Stdout()))
}

func sortAndFilterSessions(sessions []types.SessionTracker, kinds []types.SessionKind) []types.SessionTracker {
	filtered := slices.DeleteFunc(sessions, func(st types.SessionTracker) bool {
		return !slices.Contains(kinds, st.GetSessionKind()) ||
			(st.GetState() != types.SessionState_SessionStateRunning &&
				st.GetState() != types.SessionState_SessionStatePending)
	})
	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].GetCreated().Before(filtered[j].GetCreated())
	})
	return filtered
}

func serializeSessions(sessions []types.SessionTracker, format string, w io.Writer) error {
	switch format {
	case teleport.Text, "":
		printSessions(w, sessions)
	case teleport.JSON:
		out, err := utils.FastMarshalIndent(sessions, "", "  ")
		if err != nil {
			return trace.Wrap(err)
		}
		fmt.Fprintln(w, string(out))
	case teleport.YAML:
		out, err := yaml.Marshal(sessions)
		if err != nil {
			return trace.Wrap(err)
		}
		fmt.Fprintln(w, string(out))
	default:
		return trace.BadParameter("unsupported format %q", format)
	}
	return nil
}

func printSessions(output io.Writer, sessions []types.SessionTracker) {
	table := asciitable.MakeTable([]string{"ID", "Kind", "Created", "User", "Target", "Address"})
	for _, s := range sessions {
		table.AddRow([]string{
			s.GetSessionID(),
			string(s.GetSessionKind()),
			humanize.Time(s.GetCreated()),
			s.GetHostUser(),
			target(s),
			address(s),
		})
	}

	tableOutput := table.AsBuffer().String()
	fmt.Fprintln(output, tableOutput)
}

func address(s types.SessionTracker) string {
	switch s.GetSessionKind() {
	case types.KubernetesSessionKind:
		// address isn't populated in the session tracker for kube sessions,
		// so we display the command the exec session is running
		return strings.Join(s.GetCommand(), " ")
	default:
		return s.GetAddress()
	}
}

func target(s types.SessionTracker) string {
	switch s.GetSessionKind() {
	case types.SSHSessionKind:
		return s.GetLogin() + "@" + s.GetHostname()
	case types.KubernetesSessionKind:
		return s.GetHostname() + "@" + s.GetKubeCluster()
	case types.AppSessionKind:
		return s.GetAppName()
	case types.DatabaseSessionKind:
		return s.GetDatabaseName()
	case types.WindowsDesktopSessionKind:
		return s.GetLogin() + "@" + s.GetDesktopName()
	default:
		return s.GetHostname()
	}
}

type clusterInfo struct {
	ClusterName string            `json:"cluster_name"`
	Status      string            `json:"status"`
	ClusterType string            `json:"cluster_type"`
	Labels      map[string]string `json:"labels"`
	Selected    bool              `json:"selected"`
}

func serializeClusters(rootCluster clusterInfo, leafClusters []clusterInfo, format string) (string, error) {
	clusters := []clusterInfo{rootCluster}
	clusters = append(clusters, leafClusters...)
	var out []byte
	var err error
	if format == teleport.JSON {
		out, err = utils.FastMarshalIndent(clusters, "", "  ")
	} else {
		out, err = yaml.Marshal(clusters)
	}
	return string(out), trace.Wrap(err)
}

// accessRequestForSSH attempts to create an access request for the case
// where "tsh ssh" was attempted and access was denied
func accessRequestForSSH(ctx context.Context, cf *CLIConf, tc *client.TeleportClient) (types.AccessRequest, error) {
	if tc.Host == "" {
		return nil, trace.BadParameter("no host specified")
	}
	clt, err := tc.ConnectToCluster(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	defer clt.Close()

	rsp, err := clt.AuthClient.GetSSHTargets(ctx, &proto.GetSSHTargetsRequest{
		Host: tc.Host,
		Port: strconv.Itoa(tc.HostPort),
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}

	if len(rsp.Servers) > 1 {
		// Ambiguous hostname matches should have been handled by onSSH and
		// would not make it here, this is a sanity check. Ambiguous host ID
		// matches should be impossible.
		return nil, trace.NotFound("hostname %q is ambiguous and matches multiple nodes, unable to request access", tc.Host)
	}
	if len(rsp.Servers) == 0 {
		// Did not find any nodes by hostname or ID.
		return nil, trace.NotFound("node %q not found, unable to request access", tc.Host)
	}

	// At this point we have exactly 1 node.
	node := rsp.Servers[0]
	var req types.AccessRequest
	requestResourceIDs := []types.ResourceID{{
		ClusterName: tc.SiteName,
		Kind:        types.KindNode,
		Name:        node.GetName(),
	}}
	switch cf.RequestMode {
	case accessRequestModeRole:
		req, err = getAutoRoleRequest(ctx, clt, requestResourceIDs, tc)
	case accessRequestModeResource:
		req, err = getAutoResourceRequest(ctx, tc, requestResourceIDs)
	default:
		return nil, trace.BadParameter("unexpected request mode %q", cf.RequestMode)
	}
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return req, nil
}

func getAutoResourceRequest(ctx context.Context, tc *client.TeleportClient, requestResourceIDs []types.ResourceID) (types.AccessRequest, error) {
	// Roles to request will be automatically determined on the backend.
	req, err := services.NewAccessRequestWithResources(tc.Username, nil, requestResourceIDs)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	req.SetLoginHint(tc.HostLogin)

	// Set the DryRun flag and send the request to auth for full validation. If
	// the user has no search_as_roles or is not allowed to SSH to the host with
	// the requested login, we will get an error here.
	req.SetDryRun(true)
	req.SetRequestReason("Dry run, this request will not be created. If you see this, there is a bug.")
	if err := tc.WithRootClusterClient(ctx, func(clt authclient.ClientI) error {
		req, err = clt.CreateAccessRequestV2(ctx, req)
		return trace.Wrap(err)
	}); err != nil {
		return nil, trace.Wrap(err)
	}
	req.SetDryRun(false)
	req.SetRequestReason("")
	return req, nil
}

func getAutoRoleRequest(ctx context.Context, clt *client.ClusterClient, requestResourceIDs []types.ResourceID, tc *client.TeleportClient) (types.AccessRequest, error) {
	rootClient, err := clt.ConnectToRootCluster(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	resp, err := rootClient.GetAccessCapabilities(ctx, types.AccessCapabilitiesRequest{
		RequestableRoles:                 true,
		ResourceIDs:                      requestResourceIDs,
		Login:                            tc.HostLogin,
		FilterRequestableRolesByResource: true,
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}

	req, err := services.NewAccessRequestWithResources(tc.Username, resp.RequestableRoles, nil)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return req, nil
}

func retryWithAccessRequest(
	cf *CLIConf,
	tc *client.TeleportClient,
	fn func() error,
	onAccessRequestCreator func(ctx context.Context, cf *CLIConf, tc *client.TeleportClient) (types.AccessRequest, error),
	resource string,
) error {
	origErr := fn()
	if cf.RequestMode == accessRequestModeOff || !trace.IsAccessDenied(origErr) {
		// Return if --request-mode=off was specified.
		// Return the original error if it's not AccessDenied.
		// Quit now if we don't have a hostname.
		return trace.Wrap(origErr)
	}

	// Try to construct an access request for this resource.
	req, err := onAccessRequestCreator(cf.Context, cf, tc)
	if err != nil {
		// We can't request access to the resource or we couldn't query the ID. Log
		// a short debug message in case this is unexpected, but return the
		// original AccessDenied error from the ssh attempt which is likely to
		// be far more relevant to the user.
		logger.DebugContext(cf.Context, "Not attempting to automatically request access, reason", "error", err)
		return trace.Wrap(origErr)
	}

	// Print and log the original AccessDenied error.
	fmt.Fprintln(os.Stderr, utils.UserMessageFromError(origErr))
	fmt.Fprintf(os.Stdout, "You do not currently have access to %q, attempting to request access.\n\n", resource)
	if err := promptUserForAccessRequestDetails(cf, req); err != nil {
		return trace.Wrap(err)
	}

	if err := sendAccessRequestAndWaitForApproval(cf, tc, req); err != nil {
		return trace.Wrap(err)
	}

	// Retry now that request has been approved and certs updated.
	// Clear the original exit status.
	tc.SetExitStatus(0)
	return trace.Wrap(fn())
}

func promptUserForAccessRequestDetails(cf *CLIConf, req types.AccessRequest) error {
	if cf.RequestMode == accessRequestModeRole {
		// If this is a role access request, ensure that it only has one role.
		switch len(req.GetRoles()) {
		case 0:
			return trace.AccessDenied("no roles to request that would grant access")
		case 1:
			// No need to choose a role, just set request reason.
		default:
			selectedRole, err := prompt.PickOne(
				cf.Context, os.Stdout, prompt.NewContextReader(os.Stdin),
				"Choose role to request",
				req.GetRoles())
			if err != nil {
				return trace.Wrap(err)
			}
			req.SetRoles([]string{selectedRole})
		}
	}

	if err := setAccessRequestReason(cf, req); err != nil {
		return trace.Wrap(err)
	}

	return nil
}

func setAccessRequestReason(cf *CLIConf, req types.AccessRequest) (err error) {
	requestReason := cf.RequestReason
	if requestReason == "" {
		// Prompt for a request reason.
		requestReason, err = prompt.Input(cf.Context, os.Stdout, prompt.Stdin(), "Enter request reason")
		if err != nil {
			return trace.Wrap(err)
		}
	}
	req.SetRequestReason(requestReason)
	return nil
}

func sendAccessRequestAndWaitForApproval(cf *CLIConf, tc *client.TeleportClient, req types.AccessRequest) (err error) {
	cf.RequestID = req.GetName()
	fmt.Fprint(os.Stdout, "Creating request...\n")
	// Always create access request against the root cluster.
	if err := tc.WithRootClusterClient(cf.Context, func(clt authclient.ClientI) error {
		req, err = clt.CreateAccessRequestV2(cf.Context, req)
		return trace.Wrap(err)
	}); err != nil {
		return trace.Wrap(err)
	}

	if cf.Username == "" {
		cf.Username = tc.Username
	}
	// re-fetch the request to display it with roles populated.
	onRequestShow(cf)
	fmt.Println("")

	// Wait for the request to be resolved.
	fmt.Fprintf(os.Stdout, "Waiting for request approval...\n")
	var resolvedReq types.AccessRequest
	if err := tc.WithRootClusterClient(cf.Context, func(clt authclient.ClientI) error {
		resolvedReq, err = awaitRequestResolution(cf.Context, clt, req)
		return trace.Wrap(err)
	}); err != nil {
		return trace.Wrap(err)
	}

	// Handle resolution and update client certs if approved.
	if err := onRequestResolution(cf, tc, resolvedReq); err != nil {
		return trace.Wrap(err)
	}
	return nil
}

func onSSHLatency(cf *CLIConf) error {
	tc, err := makeClient(cf)
	if err != nil {
		return trace.Wrap(err)
	}

	clt, err := tc.ConnectToCluster(cf.Context)
	if err != nil {
		return trace.Wrap(err)
	}
	defer clt.Close()

	target, err := tc.GetTargetNode(cf.Context, clt.AuthClient, nil)
	if err != nil {
		return trace.Wrap(err)
	}

	nodeClient, err := tc.ConnectToNode(
		cf.Context,
		clt,
		client.NodeDetails{Addr: target.Addr, Cluster: tc.SiteName},
		tc.Config.HostLogin,
	)
	if err != nil {
		tc.SetExitStatus(1)
		return trace.Wrap(err)
	}
	defer nodeClient.Close()

	targetPinger, err := latency.NewSSHPinger(nodeClient.Client)
	if err != nil {
		return trace.Wrap(err)
	}

	return trace.Wrap(showLatency(cf.Context, clt.ProxyClient, targetPinger, "Proxy", tc.Host))
}

// Executes the given command on the client machine (localhost). If no command is given,
// executes shell
func runLocalCommand(hostLogin string, command []string) error {
	if len(command) == 0 {
		if hostLogin == "" {
			user, err := user.Current()
			if err != nil {
				return trace.Wrap(err)
			}
			hostLogin = user.Username
		}
		shell, err := shell.GetLoginShell(hostLogin)
		if err != nil {
			return trace.Wrap(err)
		}
		command = []string{shell}
	}

	cmd := exec.Command(command[0], command[1:]...)
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	return cmd.Run()
}

// onResolve executes `tsh resolve`, a command that
// attempts to resolve a single host from a provided
// hostname. The host information provided may be
// interpolated by proxy templates and converted
// from a hostname into a fuzzy search, or predicate query.
// Errors are returned if unable to connect to the cluster,
// no matching hosts were found, or multiple matching hosts
// were found. This is primarily meant to be used as a command
// for a match exec block in an SSH config.
func onResolve(cf *CLIConf) error {
	tc, err := makeClient(cf)
	if err != nil {
		return trace.Wrap(err)
	}

	req := proto.ListUnifiedResourcesRequest{
		Kinds:               []string{types.KindNode},
		Labels:              tc.Labels,
		SearchKeywords:      tc.SearchKeywords,
		PredicateExpression: tc.PredicateExpression,
		UseSearchAsRoles:    tc.UseSearchAsRoles,
		SortBy:              types.SortBy{Field: types.ResourceKind},
		// Limit to 2 so we can check for an ambiguous result
		Limit: 2,
	}

	// If no search criteria were explicitly provided, then match exclusively
	// on the hostname of the server. Otherwise, this would end up listing
	// the first two servers that the user has access to and yield unexpected results.
	if len(tc.Labels) == 0 && len(tc.SearchKeywords) == 0 && tc.PredicateExpression == "" {
		req.PredicateExpression = fmt.Sprintf(`name == "%s"`, tc.Host)
	}

	// Only enable the re-authentication behavior if not invoked with `-q`. When
	// in quiet mode, this command is likely being invoked via ssh and
	// the login prompt will not be able to be presented to users anyway.
	executor := client.RetryWithRelogin
	if cf.Quiet {
		executor = func(ctx context.Context, teleportClient *client.TeleportClient, f func() error, option ...client.RetryWithReloginOption) error {
			return f()
		}
	}

	var page []*types.EnrichedResource
	if err := executor(cf.Context, tc, func() error {
		clt, err := tc.ConnectToCluster(cf.Context)
		if err != nil {
			return trace.Wrap(err)
		}

		defer clt.Close()

		page, _, err = apiclient.GetUnifiedResourcePage(cf.Context, clt.AuthClient, &req)
		if err != nil {
			return trace.Wrap(err)
		}

		return nil
	}); err != nil {
		return trace.Wrap(err)
	}

	switch len(page) {
	case 1:
	case 0:
		return trace.NotFound("no matching hosts found")
	default:
		return trace.BadParameter("multiple matching hosts found")
	}

	if cf.Quiet {
		return nil
	}

	format := strings.ToLower(cf.Format)
	switch format {
	case teleport.Text, "":
		printNodesAsText(cf.Stdout(), []types.Server{page[0].ResourceWithLabels.(types.Server)}, true)
	case teleport.JSON:
		utils.WriteJSON(cf.Stdout(), page[0].ResourceWithLabels)
	case teleport.YAML:
		utils.WriteYAML(cf.Stdout(), page[0].ResourceWithLabels)
	default:
		return trace.BadParameter("unsupported format %q", cf.Format)
	}

	return nil
}

// onSSH executes 'tsh ssh' command
func onSSH(cf *CLIConf, initFunc ClientInitFunc) error {
	// Handle fork after authentication.
	var disownSignal *os.File
	var forkAuthSuccessful atomic.Bool
	if cf.isForkAuthChild() {
		ctx, cancel := context.WithCancel(cf.Context)
		cf.Context = ctx
		// Prep files.
		disownSignal = newSignalFile(cf.forkSignalFd)
		defer disownSignal.Close()
		killSignal := newSignalFile(cf.forkKillFd)
		defer killSignal.Close()

		// Watch kill signal to check when parent exits. If the read returns before
		// the child finishes authentication, the parent has died and the child
		// needs to die too.
		go func() {
			err := <-reexec.NotifyFileSignal(killSignal)
			if err != nil && !forkAuthSuccessful.Load() {
				cancel()
			}
		}()
	}

	// If "tsh ssh -V" is invoked, tsh is in OpenSSH compatibility mode, show
	// the version and exit.
	if cf.ShowVersion {
		modules.GetModules().PrintVersion()
		return nil
	}

	// If "tsh ssh" is invoked with the "-t" or "-T" flag, manually validate
	// "-t" and "-T" flags for "tsh ssh" due to the lack of inverse short flags
	// in kingpin.
	if cf.Interactive && cf.NonInteractive {
		return trace.BadParameter("either -t or -T can be specified, not both")
	}
	if cf.NonInteractive {
		cf.Interactive = false
	}

	// If "tsh ssh" is invoked the user must specify some host to connect to.
	// In the past, this was handled by making "UserHost" required in kingpin.
	// However, to support "tsh ssh -V" this was no longer possible. This
	// property is how enforced in this function.
	if cf.UserHost == "" {
		return trace.BadParameter("required argument '[user@]host' not provided")
	}

	tc, err := initFunc(cf)
	if err != nil {
		return trace.Wrap(err)
	}

	tc.AllowHeadless = true

	// Support calling `tsh ssh -- <command>` (with a double dash before the command)
	if len(cf.RemoteCommand) > 0 && strings.TrimSpace(cf.RemoteCommand[0]) == "--" {
		cf.RemoteCommand = cf.RemoteCommand[1:]
	}

	tc.Stdin = cf.Stdin()
	err = retryWithAccessRequest(cf, tc, func() error {
		sshFunc := func() error {
			var opts []func(*client.SSHOptions)
			if cf.LocalExec {
				opts = append(opts, client.WithLocalCommandExecutor(runLocalCommand))
			}

			if disownSignal != nil {
				opts = append(opts, client.WithForkAfterAuthentication(func() error {
					newStdin, err := replaceStdin()
					if err != nil {
						return trace.Wrap(err)
					}
					tc.Stdin = newStdin
					forkAuthSuccessful.Store(true)
					return trace.Wrap(reexec.SignalAndClose(disownSignal))
				}))
			}

			return tc.SSH(cf.Context, cf.RemoteCommand, opts...)
		}
		if !cf.Relogin {
			err = sshFunc()
		} else {
			err = client.RetryWithRelogin(cf.Context, tc, sshFunc)
		}
		if err != nil {
			if errors.Is(err, teleport.ErrNodeIsAmbiguous) ||
				// TODO(tross) DELETE IN v20.0.0
				strings.Contains(utils.UserMessageFromError(err), teleport.NodeIsAmbiguous) {
				clt, err := tc.ConnectToCluster(cf.Context)
				if err != nil {
					return trace.Wrap(err)
				}
				rsp, err := clt.AuthClient.GetSSHTargets(cf.Context, &proto.GetSSHTargetsRequest{
					Host: tc.Host,
					Port: strconv.Itoa(tc.HostPort),
				})
				if err != nil {
					return trace.Wrap(err)
				}
				fmt.Fprintf(cf.Stderr(), "error: ambiguous host could match multiple nodes\n\n")
				printNodesAsText(cf.Stderr(), rsp.Servers, true)
				fmt.Fprintf(cf.Stderr(), "Hint: try addressing the node by unique id (ex: tsh ssh user@node-id)\n")
				fmt.Fprintf(cf.Stderr(), "Hint: use 'tsh ls -v' to list all nodes with their unique ids\n")
				fmt.Fprintf(cf.Stderr(), "\n")
				return trace.Wrap(&common.ExitCodeError{Code: 1})
			}
			return trace.Wrap(err)
		}
		return nil
	},
		accessRequestForSSH,
		fmt.Sprintf("%s@%s", tc.HostLogin, tc.Host),
	)

	// Exit with the same exit status as the failed command.
	return trace.Wrap(convertSSHExitCode(tc, err))
}

func convertSSHExitCode(tc *client.TeleportClient, err error) error {
	if status := tc.ExitStatus(); status != 0 {
		var exitErr *common.ExitCodeError
		if errors.As(err, &exitErr) {
			// Already have an exitCodeError, return that.
			return trace.Wrap(err)
		}
		if err != nil {
			// Print the error here so we don't lose it when returning the exitCodeError.
			fmt.Fprintln(tc.Stderr, utils.UserMessageFromError(err))
		}
		err = &common.ExitCodeError{Code: status}
		return trace.Wrap(err)
	}
	return trace.Wrap(err)
}

// onBenchmark executes benchmark
func onBenchmark(cf *CLIConf, suite benchmark.Suite) error {
	tc, err := makeClient(cf)
	if err != nil {
		return trace.Wrap(err)
	}
	cnf := benchmark.Config{
		MinimumWindow: cf.BenchDuration,
		Rate:          cf.BenchRate,
	}

	result, err := cnf.Benchmark(cf.Context, tc, suite)
	if err != nil {
		fmt.Fprintln(os.Stderr, utils.UserMessageFromError(err))
		return trace.Wrap(&common.ExitCodeError{Code: 255})
	}
	fmt.Fprintf(cf.Stdout(), "\n")
	fmt.Fprintf(cf.Stdout(), "* Requests originated: %v\n", result.RequestsOriginated)
	fmt.Fprintf(cf.Stdout(), "* Requests failed: %v\n", result.RequestsFailed)
	if result.LastError != nil {
		fmt.Fprintf(cf.Stdout(), "* Last error: %v\n", result.LastError)
	}
	fmt.Fprintf(cf.Stdout(), "\nHistogram\n\n")
	t := asciitable.MakeTable([]string{"Percentile", "Response Duration"})
	for _, quantile := range []float64{25, 50, 75, 90, 95, 99, 100} {
		t.AddRow([]string{
			fmt.Sprintf("%v", quantile),
			fmt.Sprintf("%v ms", result.Histogram.ValueAtQuantile(quantile)),
		})
	}
	if _, err := io.Copy(cf.Stdout(), t.AsBuffer()); err != nil {
		return trace.Wrap(err)
	}
	fmt.Fprintf(cf.Stdout(), "\n")
	if cf.BenchExport {
		path, err := benchmark.ExportLatencyProfile(cf.Context, cf.BenchExportPath, result.Histogram, cf.BenchTicks, cf.BenchValueScale)
		if err != nil {
			fmt.Fprintf(cf.Stderr(), "failed exporting latency profile: %s\n", utils.UserMessageFromError(err))
		} else {
			fmt.Fprintf(cf.Stdout(), "latency profile saved: %v\n", path)
		}
	}
	return nil
}

// onJoin executes 'ssh join' command
func onJoin(cf *CLIConf) error {
	// TODO(espadolini): figure out if connection resumption should be allowed
	// on join, and if so, for which participant modes
	cf.DisableSSHResumption = true
	if err := validateParticipantMode(types.SessionParticipantMode(cf.JoinMode)); err != nil {
		return trace.Wrap(err)
	}

	cf.NodeLogin = teleport.SSHSessionJoinPrincipal
	tc, err := makeClient(cf)
	if err != nil {
		return trace.Wrap(err)
	}
	sid, err := session.ParseID(cf.SessionID)
	if err != nil {
		return trace.BadParameter("'%v' is not a valid session ID (must be GUID)", cf.SessionID)
	}
	err = client.RetryWithRelogin(cf.Context, tc, func() error {
		return tc.Join(cf.Context, types.SessionParticipantMode(cf.JoinMode), *sid, nil)
	})
	if err != nil {
		return trace.Wrap(err)
	}
	return nil
}

// onSCP executes 'tsh scp' command
func onSCP(cf *CLIConf) error {
	if len(cf.CopySpec) < 2 {
		return trace.Errorf("local and remote destinations are required")
	}

	tc, err := makeClient(cf)
	if err != nil {
		return trace.Wrap(err)
	}

	tc.AllowHeadless = true

	// allow the file transfer to be gracefully stopped if the user wishes
	ctx, cancel := signal.NotifyContext(cf.Context, os.Interrupt)
	cf.Context = ctx
	defer cancel()

	executor := client.RetryWithRelogin
	if !cf.Relogin {
		executor = func(ctx context.Context, teleportClient *client.TeleportClient, f func() error, option ...client.RetryWithReloginOption) error {
			return f()
		}
	}

	err = executor(cf.Context, tc, func() error {
		return trace.Wrap(tc.SFTP(
			cf.Context,
			cf.CopySpec[:len(cf.CopySpec)-1],
			cf.CopySpec[len(cf.CopySpec)-1],
			sftp.Options{
				Recursive:      cf.RecursiveCopy,
				PreserveAttrs:  cf.PreserveAttrs,
				Quiet:          cf.Quiet,
				ProgressWriter: cf.Stdout(),
			},
		))
	})

	// don't print context canceled errors to the user
	if err == nil || errors.Is(err, context.Canceled) {
		return nil
	}

	return trace.Wrap(err)
}

// makeClient takes the command-line configuration and constructs & returns
// a fully configured TeleportClient object
func makeClient(cf *CLIConf) (*client.TeleportClient, error) {
	tc, err := makeClientForProxy(cf, cf.Proxy)
	return tc, trace.Wrap(err)
}

// wrapInitClientWithUpdateCheck wraps the client initialization function to the Teleport cluster,
// adding a managed update check immediately after the connection is established.
func wrapInitClientWithUpdateCheck(clientInitFunc ClientInitFunc, reExecArgs []string) ClientInitFunc {
	return func(cf *CLIConf) (*client.TeleportClient, error) {
		tc, err := clientInitFunc(cf)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		if cf.checkManagedUpdates {
			if err := tools.CheckAndUpdateRemote(cf.Context, tc.WebProxyAddr, tc.InsecureSkipVerify, reExecArgs); err != nil {
				return nil, trace.Wrap(err)
			}
		}
		return tc, nil
	}
}

// makeClient takes the command-line configuration and a proxy address and constructs & returns
// a fully configured TeleportClient object
func makeClientForProxy(cf *CLIConf, proxy string) (*client.TeleportClient, error) {
	c, err := loadClientConfigFromCLIConf(cf, proxy)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	ctx, span := c.Tracer.Start(cf.Context, "makeClientForProxy/init")
	defer span.End()

	tc, err := client.NewClient(c)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Load SSH key for the cluster indicated in the profile.
	// Handle gracefully if the profile is empty, the key cannot
	// be found, or the key isn't supported as an agent key.
	profile, profileError := c.GetProfile(proxy)
	if profileError == nil {
		if err := tc.LoadKeyForCluster(ctx, profile.SiteName); err != nil {
			if !trace.IsNotFound(err) && !trace.IsConnectionProblem(err) && !trace.IsCompareFailed(err) {
				return nil, trace.Wrap(err)
			}
			logger.InfoContext(ctx, "Could not load key for cluster into the local agent",
				"cluster", cf.SiteName,
				"error", err,
			)
		}
	}

	// If we are missing client profile information, ping the webproxy
	// for proxy info and load it into the client config.
	if profileError != nil || profile.MissingClusterDetails {
		logger.DebugContext(cf.Context, "Pinging the proxy to fetch listening addresses for non-web ports")
		_, err := tc.Ping(cf.Context)
		if err != nil {
			return nil, trace.Wrap(err)
		}

		// This is a placeholder profile created from limited cluster details.
		// Save missing cluster details gathererd during Ping.
		if profileError == nil && profile.MissingClusterDetails {
			if err := tc.SaveProfile(true); err != nil {
				return nil, trace.Wrap(err)
			}
		}
	}

	return tc, nil
}

func loadClientConfigFromCLIConf(cf *CLIConf, proxy string) (*client.Config, error) {
	if cf.TracingProvider == nil {
		cf.TracingProvider = tracing.NoopProvider()
		cf.tracer = cf.TracingProvider.Tracer(teleport.ComponentTSH)
	}

	ctx, span := cf.tracer.Start(cf.Context, "loadClientConfigFromCLIConf")
	defer span.End()

	// Parse OpenSSH style options.
	options, err := parseOptions(cf.Options)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// split login & host
	hostLogin := cf.NodeLogin
	hostUser := cf.UserHost
	var labels map[string]string
	if hostUser != "" {
		parts := strings.Split(hostUser, "@")
		partsLength := len(parts)
		if partsLength > 1 {
			hostLogin = strings.Join(parts[:partsLength-1], "@")
			hostUser = parts[partsLength-1]
		}
		// see if remote host is specified as a set of labels
		if strings.Contains(hostUser, "=") {
			labels, err = client.ParseLabelSpec(hostUser)
			if err != nil {
				return nil, trace.Wrap(err)
			}
		}
	} else if cf.CopySpec != nil {
		for _, location := range cf.CopySpec {
			// Extract username and host from "username@host:file/path"
			userHost, _, found := strings.Cut(location, ":")
			if !found {
				continue
			}

			login, hostname, found := strings.Cut(userHost, "@")
			if found {
				hostLogin = login
				hostUser = hostname
			} else {
				hostUser = userHost
			}
			break

		}
	}

	// explicitly passed --labels overrides user@labels positional arg form.
	if cf.Labels != "" {
		labels, err = client.ParseLabelSpec(cf.Labels)
		if err != nil {
			return nil, trace.Wrap(err)
		}
	}

	fPorts, err := client.ParsePortForwardSpec(cf.LocalForwardPorts)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	dPorts, err := client.ParseDynamicPortForwardSpec(cf.DynamicForwardedPorts)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	rPorts, err := client.ParsePortForwardSpec(cf.RemoteForwardPorts)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// 1: start with the defaults
	c := &client.Config{}
	c.DialOpts = append(c.DialOpts, metadata.WithUserAgentFromTeleportComponent(teleport.ComponentTSH))
	c.Tracer = cf.tracer

	// Force the use of proxy template below.
	useProxyTemplate := strings.Contains(cf.ProxyJump, "{{proxy}}")
	if useProxyTemplate {
		// clear proxy jump so it can be overwritten below
		cf.ProxyJump = ""
	}

	c.Host = hostUser
	c.HostPort = int(cf.NodePort)

	// Host may be either %h or %h:%p depending on the command. Proxy
	// templates match on %h:%p, so we get the full host name here.
	fullHostName := c.Host
	if _, _, err := net.SplitHostPort(fullHostName); err != nil {
		fullHostName = net.JoinHostPort(c.Host, strconv.Itoa(c.HostPort))
	}

	// Check if this host has a matching proxy template.
	expanded, tMatched := cf.TSHConfig.ProxyTemplates.Apply(fullHostName)
	if !tMatched && useProxyTemplate {
		return nil, trace.BadParameter("proxy jump contains {{proxy}} variable but did not match any of the templates in tsh config")
	} else if tMatched {
		if expanded.Host != "" {
			c.Host = expanded.Host
			logger.DebugContext(ctx, "Will connect to host as dictated by proxy template", "host", expanded.Host)

			if host, port, err := net.SplitHostPort(c.Host); err == nil {
				c.Host = host
				c.HostPort, err = strconv.Atoi(port)
				if err != nil {
					return nil, trace.Wrap(err)
				}
			}
		} else if expanded.Query != "" {
			logger.DebugContext(cf.Context, "Will query for hosts as dictated by proxy template.", "query", expanded.Query)
			cf.PredicateExpression = expanded.Query
			// The PredicateExpression is ignored if the Host is populated.
			c.Host = ""
		} else if expanded.Search != "" {
			logger.DebugContext(cf.Context, "Will search for hosts as dictated by proxy template", "search", expanded.Search)
			cf.SearchKeywords = expanded.Search
			// The SearchKeywords are ignored if the Host is populated.
			c.Host = ""
		}

		// Don't overwrite proxy jump if explicitly provided
		if cf.ProxyJump == "" && expanded.Proxy != "" {
			cf.ProxyJump = expanded.Proxy
			logger.DebugContext(cf.Context, "Will connect to proxy as dictated by  proxy template", "proxy", expanded.Proxy)
		}

		if expanded.Cluster != "" {
			cf.SiteName = expanded.Cluster
			logger.DebugContext(cf.Context, "Will connect to cluster as dictated by proxy template", "cluster", expanded.Cluster)
		}
	}

	// ProxyJump is an alias of Proxy flag
	if cf.ProxyJump != "" {
		hosts, err := utils.ParseProxyJump(cf.ProxyJump)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		c.JumpHosts = hosts
	}

	// --headless is shorthand for --auth=headless
	if cf.Headless {
		if cf.AuthConnector != "" && cf.AuthConnector != constants.HeadlessConnector {
			return nil, trace.BadParameter("either --headless or --auth can be specified, not both")
		}
		cf.AuthConnector = constants.HeadlessConnector
	}

	if cf.AuthConnector == constants.HeadlessConnector {
		// When using Headless, check for missing proxy/user/cluster values from the teleport session env variables.
		if cf.Proxy == "" {
			cf.Proxy = os.Getenv(teleport.SSHSessionWebProxyAddr)
		}
		if cf.Username == "" {
			cf.Username = os.Getenv(teleport.SSHTeleportUser)
		}
		if cf.SiteName == "" {
			cf.SiteName = os.Getenv(teleport.SSHTeleportClusterName)
		}

		// When using Headless, user must be provided.
		if cf.Username == "" {
			return nil, trace.BadParameter("user must be provided for headless login")
		}
	}

	if err := tryLockMemory(cf); err != nil {
		return nil, trace.Wrap(err)
	}

	if cf.PIVSlot != "" {
		c.PIVSlot = hardwarekey.PIVSlotKeyString(cf.PIVSlot)
		if err = c.PIVSlot.Validate(); err != nil {
			return nil, trace.Wrap(err)
		}
	}

	c.ClientStore = cf.getClientStore()

	// If the client store was initialized for the identity file, but the wrong (or missing)
	// proxy address, re-load the identity file for the provided proxy address.
	if cf.IdentityFileIn != "" && cf.Proxy != proxy {
		if err = identityfile.LoadIdentityFileIntoClientStore(c.ClientStore, cf.IdentityFileIn, proxy, c.SiteName); err == nil {
			return nil, trace.Wrap(err)
		}
	}

	// load profile. if no --proxy is given the currently active profile is used, otherwise
	// fetch profile for exact proxy we are trying to connect to.
	profileErr := c.LoadProfile(proxy)
	if profileErr != nil && !trace.IsNotFound(profileErr) {
		fmt.Printf("WARNING: Failed to load tsh profile for %q: %v\n", proxy, profileErr)
	}

	if cf.Username != "" {
		c.Username = cf.Username
	}
	c.ExplicitUsername = cf.ExplicitUsername
	// if proxy is set, and proxy is not equal to profile's
	// loaded addresses, override the values
	if err := setClientWebProxyAddr(ctx, cf, c); err != nil {
		return nil, trace.Wrap(err)
	}

	if c.ExtraProxyHeaders == nil {
		c.ExtraProxyHeaders = map[string]string{}
	}
	for _, proxyHeaders := range cf.TSHConfig.ExtraHeaders {
		proxyGlob := utils.GlobToRegexp(proxyHeaders.Proxy)
		proxyRegexp, err := regexp.Compile(proxyGlob)
		if err != nil {
			return nil, trace.Wrap(err, "invalid proxy glob %q in tsh configuration file", proxyGlob)
		}
		if proxyRegexp.MatchString(c.WebProxyAddr) {
			maps.Copy(c.ExtraProxyHeaders, proxyHeaders.Headers)
		}
	}

	if len(fPorts) > 0 {
		c.LocalForwardPorts = fPorts
	}
	if len(dPorts) > 0 {
		c.DynamicForwardedPorts = dPorts
	}
	if len(rPorts) > 0 {
		c.RemoteForwardPorts = rPorts
	}
	if cf.SiteName != "" {
		c.SiteName = cf.SiteName
	}
	if cf.KubernetesCluster != "" {
		c.KubernetesCluster = cf.KubernetesCluster
	}
	if cf.DatabaseService != "" {
		c.DatabaseService = cf.DatabaseService
	}
	if hostLogin != "" {
		c.HostLogin = hostLogin
	}
	c.Labels = labels
	c.KeyTTL = time.Minute * time.Duration(cf.MinsToLive)
	c.InsecureSkipVerify = cf.InsecureSkipVerify
	c.PredicateExpression = cf.PredicateExpression
	if cf.SearchKeywords != "" {
		c.SearchKeywords = client.ParseSearchKeywords(cf.SearchKeywords, ',')
	}

	// If a TTY was requested, make sure to allocate it. Note this applies to
	// "exec" command because a shell always has a TTY allocated.
	if cf.Interactive || options.RequestTTY {
		c.InteractiveCommand = true
	}

	if !cf.NoCache {
		c.CachePolicy = &client.CachePolicy{}
	}

	// check version compatibility of the server and client
	c.CheckVersions = !cf.SkipVersionCheck

	// parse compatibility parameter
	certificateFormat, err := parseCertificateCompatibilityFlag(cf.Compatibility, cf.CertificateFormat)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	c.CertificateFormat = certificateFormat

	// copy the authentication connector over
	if cf.AuthConnector != "" {
		c.AuthConnector = cf.AuthConnector
	}
	mfaOpts, err := parseMFAMode(cf.MFAMode)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	c.AuthenticatorAttachment = mfaOpts.AuthenticatorAttachment
	c.PreferOTP = mfaOpts.PreferOTP
	c.PreferSSO = mfaOpts.PreferSSO

	// If agent forwarding was specified on the command line enable it.
	c.ForwardAgent = options.ForwardAgent
	if cf.ForwardAgent {
		c.ForwardAgent = client.ForwardAgentYes
	}

	if err := setX11Config(c, cf, options); err != nil {
		logger.InfoContext(ctx, "X11 forwarding is not properly configured, continuing without it", "error", err)
	}

	// send variables from user env
	setEnvVariables(c, options)

	// If the caller does not want to check host keys, pass in a insecure host
	// key checker.
	if !options.StrictHostKeyChecking {
		c.HostKeyCallback = client.InsecureSkipHostKeyChecking
	}
	c.BindAddr = cf.BindAddr
	if cf.CallbackAddr != "" && cf.BindAddr == "" {
		return nil, trace.BadParameter("--callback must be used with --bind-addr")
	}
	c.CallbackAddr = cf.CallbackAddr

	// Don't execute remote command, used when port forwarding.
	c.NoRemoteExec = cf.NoRemoteExec

	// Allow the default browser used to open tsh login links to be overridden
	// (not currently implemented) or set to 'none' to suppress browser opening entirely.
	c.Browser = cf.Browser

	c.AddKeysToAgent = cf.AddKeysToAgent
	if !cf.UseLocalSSHAgent {
		c.AddKeysToAgent = client.AddKeysToAgentNo
	}

	// avoid adding keys to agent when using an identity file.
	if (cf.IdentityFileOut != "" || cf.IdentityFileIn != "") && c.AddKeysToAgent == client.AddKeysToAgentAuto {
		c.AddKeysToAgent = client.AddKeysToAgentNo
	}

	// Identity files don't support split SSH/TLS keys.
	if cf.IdentityFileOut != "" && cf.IdentityFormat == identityfile.FormatFile {
		c.GenerateUnifiedKey = true
	}

	// headless login produces short-lived MFA-verifed certs, which should never be added to the agent.
	if cf.AuthConnector == constants.HeadlessConnector {
		if cf.AddKeysToAgent == client.AddKeysToAgentYes || cf.AddKeysToAgent == client.AddKeysToAgentOnly {
			logger.InfoContext(ctx, "Skipping adding keys to agent for headless login")
		}
		c.AddKeysToAgent = client.AddKeysToAgentNo
	}

	c.DisableEscapeSequences = !cf.EnableEscapeSequences

	// pass along mock functions if provided (only used in tests)
	c.MockSSOLogin = cf.MockSSOLogin
	c.MockHeadlessLogin = cf.MockHeadlessLogin
	c.DTAuthnRunCeremony = cf.DTAuthnRunCeremony
	c.DTAutoEnroll = cf.DTAutoEnroll
	c.WebauthnLogin = cf.WebauthnLogin

	// pass along MySQL/Postgres path overrides (only used in tests).
	c.OverrideMySQLOptionFilePath = cf.overrideMySQLOptionFilePath
	c.OverridePostgresServiceFilePath = cf.overridePostgresServiceFilePath

	if cf.IdentityFileIn != "" {
		c.NonInteractive = true
	}

	c.Stderr = cf.Stderr()
	c.Stdout = cf.Stdout()

	c.Reason = cf.Reason
	c.Invited = cf.Invited
	c.DisplayParticipantRequirements = cf.displayParticipantRequirements
	c.SSHLogDir = cf.SSHLogDir
	c.DisableSSHResumption = cf.DisableSSHResumption
	return c, nil
}

// setEnvVariables configures extra env variables to send in client config based on the requested options.
// We match OpenSSH behavior: if the requested env var is not set (os.LookupEnv return false), we won't send it.
func setEnvVariables(c *client.Config, options Options) {
	if c.ExtraEnvs == nil {
		c.ExtraEnvs = map[string]string{}
	}
	for _, variable := range options.SendEnvVariables {
		value, found := os.LookupEnv(variable)
		if found {
			c.ExtraEnvs[variable] = value
		}
	}
}

// setClientStore sets the client store. If the client store was already set,
// it returns an error instead, so this should not be used after initClientStore.
func (c *CLIConf) setClientStore(store *client.Store) error {
	if !atomic.CompareAndSwapInt32(&c.clientStoreSet, 0, 1) {
		return trace.AlreadyExists("setClientStore: client store is already set; this is a bug")
	}
	c.clientStore = store
	return nil
}

// getClientStore gets the client store, initializing it if needed. This should be
// preferred over using clientStore directly in cases where it might not be initialized.
func (c *CLIConf) getClientStore() *client.Store {
	c.initClientStore()
	return c.clientStore
}

// initClientStore initializes the client identity store which will be used by the
// client to interface with client identity material. After the first call to
// initClientStore, further calls will be a no-op.
func (c *CLIConf) initClientStore() {
	if !atomic.CompareAndSwapInt32(&c.clientStoreSet, 0, 1) {
		// client store already initialized.
		return
	}

	var hwks hardwarekey.Service
	if c.disableHardwareKeyAgentClient {
		hwks = piv.NewYubiKeyService(nil /*prompt*/)
	} else {
		hwks = libhwk.NewService(c.Context, nil /*prompt*/)
	}

	switch {
	case c.IdentityFileIn != "", c.IdentityFileOut != "", c.AuthConnector == constants.HeadlessConnector:
		// Store client keys in memory, where they can be exported to non-standard
		// FS formats (e.g. identity file) or used for a single client call in memory.
		c.clientStore = client.NewMemClientStore(client.WithHardwareKeyService(hwks))

	case c.AddKeysToAgent == client.AddKeysToAgentOnly:
		// Store client keys in memory, but save trusted certs and profile to disk.
		c.clientStore = client.NewFSClientStore(c.HomePath, client.WithHardwareKeyService(hwks))
		c.clientStore.KeyStore = client.NewMemKeyStore()

	default:
		c.clientStore = client.NewFSClientStore(c.HomePath, client.WithHardwareKeyService(hwks))
	}

	// If an identity file is provided, opportunistically try to load it into the keystore. It may
	// fail if the user did not provide the --proxy flag, but in some cases the proxy, the proxy
	// address will be provided later on and the client will attempt to load the identity file then.
	if c.IdentityFileIn != "" {
		if err := identityfile.LoadIdentityFileIntoClientStore(c.clientStore, c.IdentityFileIn, c.Proxy, c.SiteName); err == nil {
			logger.DebugContext(c.Context, "failed to load identity file into client store", "err", err)
		}
	}
}

func (c *CLIConf) ProfileStatus() (*client.ProfileStatus, error) {
	if c.profileStatusOverride != nil {
		return c.profileStatusOverride, nil
	}

	profile, err := c.getClientStore().ReadProfileStatus(c.Proxy)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return profile, nil
}

func (c *CLIConf) FullProfileStatus() (*client.ProfileStatus, []*client.ProfileStatus, error) {
	currentProfile, profiles, err := c.getClientStore().FullProfileStatus()
	if err != nil {
		return nil, nil, trace.Wrap(err)
	}
	return currentProfile, profiles, nil
}

// ListProfiles returns a list of profiles the current user has
// credentials for.
func (c *CLIConf) ListProfiles() ([]*client.ProfileStatus, error) {
	profileNames, err := c.getClientStore().ListProfiles()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	profiles := make([]*client.ProfileStatus, 0, len(profileNames))
	for _, profileName := range profileNames {
		status, err := c.getClientStore().ReadProfileStatus(profileName)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		profiles = append(profiles, status)
	}

	return profiles, nil
}

// GetProfile loads user profile.
func (c *CLIConf) GetProfile() (*profile.Profile, error) {
	clientStore := c.getClientStore()
	profileName, err := client.ProfileNameFromProxyAddress(clientStore, c.Proxy)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	profile, err := clientStore.GetProfile(profileName)
	return profile, trace.Wrap(err)
}

type mfaModeOpts struct {
	AuthenticatorAttachment wancli.AuthenticatorAttachment
	PreferOTP               bool
	PreferSSO               bool
}

func parseMFAMode(mode string) (*mfaModeOpts, error) {
	opts := &mfaModeOpts{}
	switch mode {
	case "", mfaModeAuto:
	case mfaModeCrossPlatform:
		opts.AuthenticatorAttachment = wancli.AttachmentCrossPlatform
	case mfaModePlatform:
		opts.AuthenticatorAttachment = wancli.AttachmentPlatform
	case mfaModeOTP:
		opts.PreferOTP = true
	case mfaModeSSO:
		opts.PreferSSO = true
	default:
		return nil, fmt.Errorf("invalid MFA mode: %q", mode)
	}
	return opts, nil
}

// setX11Config sets X11 config using CLI and SSH option flags.
func setX11Config(c *client.Config, cf *CLIConf, o Options) error {
	// X11 forwarding can be enabled with -X, -Y, or -oForwardX11=yes
	c.EnableX11Forwarding = cf.X11ForwardingUntrusted || cf.X11ForwardingTrusted || o.ForwardX11

	if c.EnableX11Forwarding && os.Getenv(x11.DisplayEnv) == "" {
		c.EnableX11Forwarding = false
		return trace.BadParameter("$DISPLAY must be set for X11 forwarding")
	}

	c.X11ForwardingTrusted = cf.X11ForwardingTrusted
	if o.ForwardX11Trusted != nil && *o.ForwardX11Trusted {
		c.X11ForwardingTrusted = true
	}

	// Set X11 forwarding timeout, prioritizing the SSH option if set.
	c.X11ForwardingTimeout = o.ForwardX11Timeout
	if c.X11ForwardingTimeout == 0 {
		c.X11ForwardingTimeout = cf.X11ForwardingTimeout
	}

	return nil
}

// defaultWebProxyPorts is the order of default proxy ports to try, in order that
// they will be tried.
var defaultWebProxyPorts = []int{
	defaults.HTTPListenPort, teleport.StandardHTTPSPort,
}

// proxyHostsErrorMsgDefault returns the error message from attempting hosts at
// different ports for the Web Proxy.
func proxyHostsErrorMsgDefault(proxyAddress string, ports []int) string {
	buf := &bytes.Buffer{}
	buf.WriteString("Teleport proxy not available at proxy address ")

	for i, port := range ports {
		if i > 0 {
			buf.WriteString(" or ")
		}
		buf.WriteString(proxyAddress)
		buf.WriteString(":")
		buf.WriteString(strconv.Itoa(port))
	}

	return buf.String()
}

// setClientWebProxyAddr configures the client WebProxyAddr and SSHProxyAddr
// configuration values. Values that are not fully specified via configuration
// or command-line options will be deduced if necessary.
//
// If successful, setClientWebProxyAddr will modify the client Config in-place.
func setClientWebProxyAddr(ctx context.Context, cf *CLIConf, c *client.Config) error {
	// If the user has specified a proxy on the command line, and one has not
	// already been specified from configuration...

	if cf.Proxy != "" && c.WebProxyAddr == "" {
		parsedAddrs, err := client.ParseProxyHost(cf.Proxy)
		if err != nil {
			return trace.Wrap(err)
		}

		proxyAddress := parsedAddrs.WebProxyAddr
		if parsedAddrs.UsingDefaultWebProxyPort {
			logger.DebugContext(ctx, "Web proxy port was not set, attempting to detect port number to use")
			timeout, cancel := context.WithTimeout(ctx, proxyDefaultResolutionTimeout)
			defer cancel()

			proxyAddress, err = pickDefaultAddr(
				timeout, cf.InsecureSkipVerify, parsedAddrs.Host, defaultWebProxyPorts)
			if err != nil {
				return trace.Wrap(err, proxyHostsErrorMsgDefault(parsedAddrs.Host, defaultWebProxyPorts))
			}
		}

		c.WebProxyAddr = proxyAddress
		c.SSHProxyAddr = parsedAddrs.SSHProxyAddr
	}

	return nil
}

func parseCertificateCompatibilityFlag(compatibility string, certificateFormat string) (string, error) {
	switch {
	// if nothing is passed in, the role will decide
	case compatibility == "" && certificateFormat == "":
		return teleport.CertificateFormatUnspecified, nil
	// supporting the old --compat format for backward compatibility
	case compatibility != "" && certificateFormat == "":
		return utils.CheckCertificateFormatFlag(compatibility)
	// new documented flag --cert-format
	case compatibility == "" && certificateFormat != "":
		return utils.CheckCertificateFormatFlag(certificateFormat)
	// can not use both
	default:
		return "", trace.BadParameter("--compat or --cert-format must be specified")
	}
}

// flattenIdentity reads an identity file and flattens it into a tsh profile on disk.
func flattenIdentity(cf *CLIConf) error {
	// Proxy address may be loaded from existing tsh profile or from --proxy flag.
	if cf.Proxy == "" {
		return trace.BadParameter("No proxy address specified, missed --proxy flag?")
	}

	// Usually, initializing the client store with an identity file would result in
	// an in-memory client store with a profile for cf.Proxy pre-loaded. Instead,
	// initialize an FS client store and load the identity file into it.
	hwks := piv.NewYubiKeyService(nil /*prompt*/)
	clientStore := client.NewFSClientStore(cf.HomePath, client.WithHardwareKeyService(hwks))
	if err := cf.setClientStore(clientStore); err != nil {
		return trace.Wrap(err)
	}

	// Load the identity file key and partial profile into the client store.
	if err := identityfile.LoadIdentityFileIntoClientStore(clientStore, cf.IdentityFileIn, cf.Proxy, cf.SiteName); err != nil {
		return trace.Wrap(err)
	}

	fmt.Printf("Successfully flattened Identity file %q into a tsh profile.\n", cf.IdentityFileIn)

	// onStatus will ping the proxy to fill in cluster profile information missing in the
	// client store, then print the login status.
	return trace.Wrap(onStatus(cf))
}

// onShow reads an identity file (a public SSH key or a cert) and dumps it to stdout
func onShow(cf *CLIConf) error {
	keyRing, err := identityfile.KeyRingFromIdentityFile(cf.IdentityFileIn, cf.Proxy, cf.SiteName)
	if err != nil {
		return trace.Wrap(err)
	}

	// unmarshal certificate bytes into a ssh.PublicKey
	cert, _, _, _, err := ssh.ParseAuthorizedKey(keyRing.Cert)
	if err != nil {
		return trace.Wrap(err)
	}

	fmt.Printf("Cert: %#v\nPriv: %#v\nPub: %#v\n", cert, keyRing.SSHPrivateKey.Signer, keyRing.SSHPrivateKey.MarshalSSHPublicKey())
	fmt.Printf("Fingerprint: %s\n", ssh.FingerprintSHA256(keyRing.SSHPrivateKey.SSHPublicKey()))
	return nil
}

// printStatus prints the status of the profile.
func printStatus(debug bool, p *profileInfo, env map[string]string, isActive bool) {
	var prefix string
	humanDuration := "EXPIRED"
	duration := time.Until(p.ValidUntil)
	if duration.Nanoseconds() > 0 {
		humanDuration = fmt.Sprintf("valid for %v", duration.Round(time.Minute))
		// If certificate is valid for less than a minute, display "<1m" instead of "0s".
		if duration < time.Minute {
			humanDuration = "valid for <1m"
		}
	}

	proxyURL := p.getProxyURLLine(isActive, env)
	cluster := p.getClusterLine(isActive, env)
	kubeCluster := p.getKubeClusterLine(isActive, env)
	if isActive {
		prefix = "> "
	} else {
		prefix = "  "
	}

	fmt.Printf("%vProfile URL:        %v\n", prefix, proxyURL)
	fmt.Printf("  Logged in as:       %v\n", p.Username)
	if len(p.ActiveRequests) != 0 {
		fmt.Printf("  Active requests:    %v\n", strings.Join(p.ActiveRequests, ", "))
	}

	if cluster != "" {
		fmt.Printf("  Cluster:            %v\n", cluster)
	}
	fmt.Printf("  Roles:              %v\n", rolesToString(debug, p.Roles))
	if debug {
		var count int
		for k, v := range p.Traits {
			if count == 0 {
				fmt.Printf("  Traits:             %v: %v\n", k, v)
			} else {
				fmt.Printf("                      %v: %v\n", k, v)
			}
			count = count + 1
		}
	}
	if len(p.Logins) > 0 {
		fmt.Printf("  Logins:             %v\n", strings.Join(p.Logins, ", "))
	}
	if p.KubernetesEnabled {
		fmt.Printf("  Kubernetes:         enabled\n")
		if kubeCluster != "" {
			fmt.Printf("  Kubernetes cluster: %q\n", kubeCluster)
		}
		if len(p.KubernetesUsers) > 0 {
			fmt.Printf("  Kubernetes users:   %v\n", strings.Join(p.KubernetesUsers, ", "))
		}
		if len(p.KubernetesGroups) > 0 {
			fmt.Printf("  Kubernetes groups:  %v\n", strings.Join(p.KubernetesGroups, ", "))
		}
	} else {
		fmt.Printf("  Kubernetes:         disabled\n")
	}
	if len(p.Databases) != 0 {
		fmt.Printf("  Databases:          %v\n", strings.Join(p.Databases, ", "))
	}
	if len(p.AllowedResourceIDs) > 0 {
		allowedResourcesStr, err := types.ResourceIDsToString(p.AllowedResourceIDs)
		if err != nil {
			logger.WarnContext(context.Background(), "failed to marshal allowed resource IDs to string", "error", err)
		} else {
			fmt.Printf("  Allowed Resources:  %s\n", allowedResourcesStr)
		}
	}
	if p.GitHubIdentity != nil {
		fmt.Printf("  GitHub username:    %s\n", p.GitHubIdentity.Username)
	}
	fmt.Printf("  Valid until:        %v [%v]\n", p.ValidUntil, humanDuration)
	fmt.Printf("  Extensions:         %v\n", strings.Join(p.Extensions, ", "))

	if debug {
		first := true
		for k, v := range p.CriticalOptions {
			if first {
				fmt.Printf("  Critical options:   %v: %v\n", k, v)
			} else {
				fmt.Printf("                      %v: %v\n", k, v)
			}
			first = false
		}
	}

	fmt.Printf("\n")
}

func isOktaRole(role string) bool {
	return strings.Contains(role, teleport.OktaReviewerRoleContext) || strings.Contains(role, teleport.OktaAccessRoleContext)
}

func rolesToString(debug bool, roles []string) string {
	sort.Strings(roles)
	var nonOktaRoles, oktaRoles []string
	for _, role := range roles {
		if isOktaRole(role) {
			oktaRoles = append(oktaRoles, role)
		} else {
			nonOktaRoles = append(nonOktaRoles, role)
		}
	}
	if len(oktaRoles) == 0 {
		return strings.Join(nonOktaRoles, ", ")
	}

	squashRolesThreshold := 9

	if !debug && len(nonOktaRoles)+len(oktaRoles) > squashRolesThreshold {
		oktaRolesText := fmt.Sprintf("and %v more Okta access list roles ...", len(oktaRoles))
		return strings.Join(append(nonOktaRoles, oktaRolesText), ", ")
	}
	// Keep okta roles at the end of list.
	out := append(nonOktaRoles, oktaRoles...)
	return strings.Join(out, ", ")
}

// printLoginInformation displays the provided profile information to the user.
func printLoginInformation(cf *CLIConf, profile *client.ProfileStatus, profiles []*client.ProfileStatus, accessListsToReview []*accesslist.AccessList) error {
	env := getTshEnv()
	active, others := makeAllProfileInfo(profile, profiles, env)

	format := strings.ToLower(cf.Format)
	switch format {
	case teleport.JSON, teleport.YAML:
		out, err := serializeProfiles(active, others, env, format)
		if err != nil {
			return trace.Wrap(err)
		}
		fmt.Println(out)
	default:
		if profile == nil && len(profiles) == 0 {
			return nil
		}

		// Print the active profile.
		if profile != nil {
			printStatus(cf.Debug, active, env, true)
		}

		// Print all other profiles.
		for _, p := range others {
			printStatus(cf.Debug, p, env, false)
		}

		// Print relevant active env vars, if they are set.
		if cf.Verbose {
			if len(env) > 0 {
				fmt.Println("Active Environment:")
			}
			for k, v := range env {
				fmt.Printf("\t%s=%s\n", k, v)
			}
		}
	}

	if len(accessListsToReview) > 0 {
		fmt.Printf("Access lists that need to be reviewed:\n")
		for _, accessList := range accessListsToReview {
			var msg string
			nextAuditDate := accessList.Spec.Audit.NextAuditDate.Format(time.DateOnly)
			if time.Now().After(accessList.Spec.Audit.NextAuditDate) {
				msg = fmt.Sprintf("review is overdue (%v)", nextAuditDate)
			} else {
				msg = fmt.Sprintf("review is required by %v", nextAuditDate)
			}
			fmt.Printf("\t%s (%v)\n", accessList.Spec.Title, msg)
		}
		fmt.Println()
	}

	return nil
}

// onStatus command shows which proxy the user is logged into and metadata
// about the certificate.
func onStatus(cf *CLIConf) error {
	// Get the status of the active profile as well as the status
	// of any other proxies the user is logged into.
	//
	// Return error if not logged in, no active profile, or expired.
	profile, profiles, err := cf.FullProfileStatus()
	if err != nil {
		if trace.IsNotFound(err) {
			return trace.NotFound("Not logged in.")
		}
		return trace.Wrap(err)
	}

	// make the teleport client and retrieve the certificate from the proxy:
	tc, err := makeClient(cf)
	if err != nil {
		logger.WarnContext(cf.Context, "Failed to make client for retrieving cluster alerts", "error", err)
		return trace.Wrap(err)
	}

	// `tsh status` should run without requiring user interaction.
	// To achieve this, we avoid remote calls that might prompt for
	// hardware key touch or require a PIN.
	hardwareKeyInteractionRequired := tc.PrivateKeyPolicy.MFAVerified()

	var accessListsToReview []*accesslist.AccessList
	if hardwareKeyInteractionRequired {
		logger.DebugContext(cf.Context, "Skipping fetching access lists to review due to Hardware Key PIN/Touch requirement")
	} else {
		accessListsToReview = cf.getAccessListsToReview(tc)
	}
	if err := printLoginInformation(cf, profile, profiles, accessListsToReview); err != nil {
		return trace.Wrap(err)
	}

	if profile == nil {
		return trace.NotFound("Not logged in.")
	}

	duration := time.Until(profile.ValidUntil)
	if !profile.ValidUntil.IsZero() && duration.Nanoseconds() <= 0 {
		return trace.NotFound("Active profile expired.")
	}

	if hardwareKeyInteractionRequired {
		logger.DebugContext(cf.Context, "Skipping cluster alerts due to Hardware Key PIN/Touch requirement")
	} else {
		if err := common.ShowClusterAlerts(cf.Context, tc, os.Stderr, nil,
			types.AlertSeverity_HIGH); err != nil {
			logger.WarnContext(cf.Context, "Failed to display cluster alerts", "error", err)
		}
	}

	return nil
}

type profileInfo struct {
	ProxyURL           string                 `json:"profile_url"`
	Username           string                 `json:"username"`
	ActiveRequests     []string               `json:"active_requests,omitempty"`
	Cluster            string                 `json:"cluster"`
	Roles              []string               `json:"roles,omitempty"`
	Traits             wrappers.Traits        `json:"traits,omitempty"`
	Logins             []string               `json:"logins,omitempty"`
	KubernetesEnabled  bool                   `json:"kubernetes_enabled"`
	KubernetesCluster  string                 `json:"kubernetes_cluster,omitempty"`
	KubernetesUsers    []string               `json:"kubernetes_users,omitempty"`
	KubernetesGroups   []string               `json:"kubernetes_groups,omitempty"`
	Databases          []string               `json:"databases,omitempty"`
	ValidUntil         time.Time              `json:"valid_until"`
	Extensions         []string               `json:"extensions,omitempty"`
	CriticalOptions    map[string]string      `json:"critical_options,omitempty"`
	AllowedResourceIDs []types.ResourceID     `json:"allowed_resources,omitempty"`
	GitHubIdentity     *client.GitHubIdentity `json:"github_identity,omitempty"`
}

func makeAllProfileInfo(active *client.ProfileStatus, others []*client.ProfileStatus, env map[string]string) (*profileInfo, []*profileInfo) {
	activeInfo := makeProfileInfo(active, env, true)
	var othersInfo []*profileInfo
	for _, p := range others {
		othersInfo = append(othersInfo, makeProfileInfo(p, env, false))
	}
	return activeInfo, othersInfo
}

func makeProfileInfo(p *client.ProfileStatus, env map[string]string, isActive bool) *profileInfo {
	if p == nil {
		return nil
	}

	// Filter out login names that were added internally.
	// These are for internal use and are not valid UNIX login names
	// because they start with a hyphen.
	var logins []string
	for _, login := range p.Logins {
		// Specifically filters out these:
		//   - api/constants.NoLoginPrefix
		//   - teleport/constants.SSHSessionJoinPrincipal
		isTeleportDefinedLogin := strings.HasPrefix(login, "-teleport-")

		if !isTeleportDefinedLogin {
			logins = append(logins, login)
		}
	}

	selectedKubeCluster, _ := kubeconfig.SelectedKubeCluster("", p.Cluster)
	out := &profileInfo{
		ProxyURL:           p.ProxyURL.String(),
		Username:           p.Username,
		ActiveRequests:     p.ActiveRequests,
		Cluster:            p.Cluster,
		Roles:              p.Roles,
		Traits:             p.Traits,
		Logins:             logins,
		KubernetesEnabled:  p.KubeEnabled,
		KubernetesCluster:  selectedKubeCluster,
		KubernetesUsers:    p.KubeUsers,
		KubernetesGroups:   p.KubeGroups,
		Databases:          p.DatabaseServices(),
		ValidUntil:         p.ValidUntil,
		Extensions:         p.Extensions,
		CriticalOptions:    p.CriticalOptions,
		AllowedResourceIDs: p.AllowedResourceIDs,
		GitHubIdentity:     p.GitHubIdentity,
	}

	// update active profile info from env
	if isActive {
		if proxy, ok := env[proxyEnvVar]; ok {
			proxyURL := url.URL{
				Scheme: "https",
				Host:   proxy,
			}
			out.ProxyURL = proxyURL.String()
		}

		if cluster, ok := env[clusterEnvVar]; ok {
			out.Cluster = cluster
		} else if siteName, ok := env[siteEnvVar]; ok {
			out.Cluster = siteName
		}

		if kubeCluster, ok := env[kubeClusterEnvVar]; ok {
			out.KubernetesCluster = kubeCluster
		}
	}
	return out
}

func (p *profileInfo) getProxyURLLine(isActive bool, env map[string]string) string {
	// indicate if active profile proxy url is shadowed by env vars.
	if isActive {
		if _, ok := env[proxyEnvVar]; ok {
			return fmt.Sprintf("%v (%v)", p.ProxyURL, proxyEnvVar)
		}
	}
	return p.ProxyURL
}

func (p *profileInfo) getClusterLine(isActive bool, env map[string]string) string {
	// indicate if active profile cluster is shadowed by env vars.
	if isActive {
		if _, ok := env[clusterEnvVar]; ok {
			return fmt.Sprintf("%v (%v)", p.Cluster, clusterEnvVar)
		} else if _, ok := env[siteEnvVar]; ok {
			return fmt.Sprintf("%v (%v)", p.Cluster, siteEnvVar)
		}
	}
	return p.Cluster
}

func (p *profileInfo) getKubeClusterLine(isActive bool, env map[string]string) string {
	// indicate if active profile kube cluster is shadowed by env vars.
	if isActive {
		// check if kube cluster env var is set and no cluster was selected by kube config
		if _, ok := env[kubeClusterEnvVar]; ok {
			return fmt.Sprintf("%v (%v)", p.KubernetesCluster, kubeClusterEnvVar)
		}
	}
	return p.KubernetesCluster
}

func serializeProfiles(profile *profileInfo, profiles []*profileInfo, env map[string]string, format string) (string, error) {
	profileData := struct {
		Active   *profileInfo      `json:"active,omitempty"`
		Profiles []*profileInfo    `json:"profiles"`
		Env      map[string]string `json:"environment,omitempty"`
	}{profile, []*profileInfo{}, env}
	profileData.Profiles = append(profileData.Profiles, profiles...)
	var out []byte
	var err error
	if format == teleport.JSON {
		out, err = utils.FastMarshalIndent(profileData, "", "  ")
	} else {
		out, err = yaml.Marshal(profileData)
	}
	if err != nil {
		return "", trace.Wrap(err)
	}
	return string(out), nil
}

func getTshEnv() map[string]string {
	env := map[string]string{}
	for _, envVar := range tshStatusEnvVars {
		if envVal, isSet := os.LookupEnv(envVar); isSet {
			env[envVar] = envVal
		}
	}
	return env
}

func awaitRequestResolution(ctx context.Context, clt authclient.ClientI, req types.AccessRequest) (types.AccessRequest, error) {
	filter := types.AccessRequestFilter{
		User: req.GetUser(),
		ID:   req.GetName(),
	}
	watcher, err := clt.NewWatcher(ctx, types.Watch{
		Name: "await-request-approval",
		Kinds: []types.WatchKind{{
			Kind:   types.KindAccessRequest,
			Filter: filter.IntoMap(),
		}},
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	defer watcher.Close()

	// Wait for OpInit event so that returned watcher is ready.
	select {
	case event := <-watcher.Events():
		if event.Type != types.OpInit {
			return nil, trace.BadParameter("failed to watch for access requests: received an unexpected event while waiting for the initial OpInit")
		}
	case <-watcher.Done():
		return nil, trace.Wrap(watcher.Error())
	}

	// get initial state of request
	reqState, err := services.GetAccessRequest(ctx, clt, req.GetName())
	if err != nil {
		return nil, trace.Wrap(err)
	}

	for {
		if !reqState.GetState().IsPending() {
			return reqState, nil
		}

		select {
		case event := <-watcher.Events():
			switch event.Type {
			case types.OpPut:
				var ok bool
				reqState, ok = event.Resource.(*types.AccessRequestV3)
				if !ok {
					return nil, trace.BadParameter("unexpected resource type %T", event.Resource)
				}
			case types.OpDelete:
				return nil, trace.Errorf("request %s has expired or been deleted...", event.Resource.GetName())
			default:
				logger.WarnContext(ctx, "Skipping unknown event type", "event_type", event.Type)
			}
		case <-watcher.Done():
			return nil, trace.Wrap(watcher.Error())
		}
	}
}

func onRequestResolution(cf *CLIConf, tc *client.TeleportClient, req types.AccessRequest) error {
	if !req.GetState().IsApproved() {
		msg := fmt.Sprintf("request %s has been set to %s", req.GetName(), req.GetState().String())
		if reason := req.GetResolveReason(); reason != "" {
			msg = fmt.Sprintf("%s, reason=%q", msg, reason)
		}
		if req.GetState().IsDenied() {
			return trace.AccessDenied("%s", msg)
		}
		return trace.Errorf("%s", msg)
	}

	msg := "\nApproval received, getting updated certificates...\n\n"
	if reason := req.GetResolveReason(); reason != "" {
		msg = fmt.Sprintf("\nApproval received, reason=%q\nGetting updated certificates...\n\n", reason)
	}
	fmt.Fprint(os.Stderr, msg)

	err := reissueWithRequests(cf, tc, []string{req.GetName()}, nil /*dropRequests*/)
	return trace.Wrap(err)
}

// reissueWithRequests handles a certificate reissue, applying new requests by ID,
// and saving the updated profile.
func reissueWithRequests(cf *CLIConf, tc *client.TeleportClient, newRequests []string, dropRequests []string) error {
	profile, err := tc.ClientStore.ReadProfileStatus(cf.Proxy)
	if err != nil {
		return trace.Wrap(err)
	}
	params := client.ReissueParams{
		AccessRequests:     newRequests,
		DropAccessRequests: dropRequests,
		RouteToCluster:     cf.SiteName,
	}
	// If the certificate already had active requests, add them to our inputs parameters.
	for _, reqID := range profile.ActiveRequests {
		if !slices.Contains(dropRequests, reqID) {
			params.AccessRequests = append(params.AccessRequests, reqID)
		}
	}
	if params.RouteToCluster == "" {
		params.RouteToCluster = profile.Cluster
	}
	if err := tc.ReissueUserCerts(cf.Context, client.CertCacheDrop, params); err != nil {
		return trace.Wrap(err)
	}
	if err := tc.SaveProfile(true); err != nil {
		return trace.Wrap(err)
	}
	if err := updateKubeConfigOnLogin(cf, tc); err != nil {
		return trace.Wrap(err)
	}
	return nil
}

func onApps(cf *CLIConf) error {
	if cf.ListAll {
		return trace.Wrap(listAppsAllClusters(cf))
	}
	tc, err := makeClient(cf)
	if err != nil {
		return trace.Wrap(err)
	}

	// Get a list of all applications.
	var apps []types.Application
	err = client.RetryWithRelogin(cf.Context, tc, func() error {
		apps, err = tc.ListApps(cf.Context, nil /* custom filter */)
		return err
	})
	if err != nil {
		return trace.Wrap(err)
	}

	// Retrieve profile to be able to show which apps user is logged into.
	profile, err := tc.ProfileStatus()
	if err != nil {
		return trace.Wrap(err)
	}

	// Sort by app name.
	sort.Slice(apps, func(i, j int) bool {
		return apps[i].GetName() < apps[j].GetName()
	})

	return trace.Wrap(showApps(apps, profile.Apps, cf.Stdout(), cf.Format, cf.Verbose))
}

type appListing struct {
	Proxy   string            `json:"proxy"`
	Cluster string            `json:"cluster"`
	App     types.Application `json:"app"`
}

func (al appListing) GetProxy() string {
	return al.Proxy
}

func (al appListing) GetCluster() string {
	return al.Cluster
}

type appListings []appListing

func (l appListings) Len() int {
	return len(l)
}

func (l appListings) Less(i, j int) bool {
	if l[i].Proxy != l[j].Proxy {
		return l[i].Proxy < l[j].Proxy
	}
	if l[i].Cluster != l[j].Cluster {
		return l[i].Cluster < l[j].Cluster
	}
	return l[i].App.GetName() < l[j].App.GetName()
}

func (l appListings) Swap(i, j int) {
	l[i], l[j] = l[j], l[i]
}

func listAppsAllClusters(cf *CLIConf) error {
	clusters, err := getClusterClients(cf, types.KindAppServer)
	if err != nil {
		return trace.Wrap(err)
	}

	defer func() {
		// close all clients
		for _, cluster := range clusters {
			_ = cluster.Close()
		}
	}()

	// Fetch listings for all clusters in parallel with an upper limit
	group, groupCtx := errgroup.WithContext(cf.Context)
	group.SetLimit(10)

	// mu guards access to dbListings
	var (
		mu       sync.Mutex
		listings appListings
		errors   []error
	)
	for _, cluster := range clusters {
		if cluster.connectionError != nil {
			mu.Lock()
			errors = append(errors, cluster.connectionError)
			mu.Unlock()
			continue
		}

		logger := logger.With("cluster", cluster.name)
		group.Go(func() error {
			servers, err := apiclient.GetAllResources[types.AppServer](groupCtx, cluster.auth, &cluster.req)
			if err != nil {
				logger.ErrorContext(groupCtx, "Failed to get app servers", "error", err)

				mu.Lock()
				errors = append(errors, trace.ConnectionProblem(err, "failed to list app serves for cluster %s: %v", cluster.name, err))
				mu.Unlock()
				return nil
			}

			apps := make([]types.Application, 0, len(servers))
			for _, srv := range servers {
				apps = append(apps, srv.GetApp())
			}
			apps = types.DeduplicateApps(apps)

			localAppListings := make([]appListing, 0, len(servers))
			for _, app := range apps {
				localAppListings = append(localAppListings, appListing{
					Proxy:   cluster.profile.ProxyURL.Host,
					Cluster: cluster.name,
					App:     app,
				})
			}

			mu.Lock()
			listings = append(listings, localAppListings...)
			mu.Unlock()

			return nil
		})
	}

	if err := group.Wait(); err != nil {
		return trace.Wrap(err)
	}

	if len(listings) == 0 && len(errors) > 0 {
		return trace.NewAggregate(errors...)
	}

	sort.Sort(listings)

	profile, err := cf.ProfileStatus()
	if err != nil {
		return trace.Wrap(err)
	}
	var active []tlsca.RouteToApp
	if profile != nil {
		active = profile.Apps
	}

	format := strings.ToLower(cf.Format)
	switch format {
	case teleport.Text, "":
		if err := writeAppTable(cf.Stdout(), listings, appTableConfig{
			listAll: true,
			active:  active,
			verbose: cf.Verbose,
		}); err != nil {
			return trace.Wrap(err)
		}

	case teleport.JSON, teleport.YAML:
		out, err := serializeAppsWithClusters(listings, format)
		if err != nil {
			return trace.Wrap(err)
		}
		if _, err := fmt.Fprintln(cf.Stdout(), out); err != nil {
			return trace.Wrap(err)
		}
	default:
		return trace.BadParameter("unsupported format %q", format)
	}
	return nil
}

func serializeAppsWithClusters(apps []appListing, format string) (string, error) {
	var out []byte
	var err error
	if format == teleport.JSON {
		out, err = utils.FastMarshalIndent(apps, "", "  ")
	} else {
		out, err = yaml.Marshal(apps)
	}
	return string(out), trace.Wrap(err)
}

func onRecordings(cf *CLIConf) error {
	fromUTC, toUTC, err := defaults.SearchSessionRange(clockwork.NewRealClock(), cf.FromUTC, cf.ToUTC, cf.recordingsSince)
	if err != nil {
		return trace.Errorf("cannot request recordings: %v", err)
	}
	tc, err := makeClient(cf)
	if err != nil {
		return trace.Wrap(err)
	}
	// Max number of days is limited to prevent too many requests being sent if dynamo is used as a backend.
	if days := toUTC.Sub(fromUTC).Hours() / 24; days > defaults.TshTctlSessionDayLimit {
		return trace.Errorf("date range for recordings listing too large: %v days specified: limit %v days",
			days, defaults.TshTctlSessionDayLimit)
	}

	var sessions []apievents.AuditEvent
	if err := client.RetryWithRelogin(cf.Context, tc, func() error {
		sessions, err = tc.SearchSessionEvents(cf.Context,
			fromUTC, toUTC, apidefaults.DefaultChunkSize,
			types.EventOrderDescending, cf.maxRecordingsToShow)
		return err
	}); err != nil {
		return trace.Wrap(err)
	}

	if err := common.ShowSessions(sessions, cf.Format, os.Stdout); err != nil {
		return trace.Wrap(err)
	}
	return nil
}

// onEnvironment handles "tsh env" command.
func onEnvironment(cf *CLIConf) error {
	profile, err := cf.ProfileStatus()
	if err != nil {
		return trace.Wrap(err)
	}

	format := strings.ToLower(cf.Format)
	switch format {
	case teleport.Text, "":
		// Print shell built-in commands to set (or unset) environment.
		switch {
		case cf.unsetEnvironment:
			fmt.Printf("unset %v\n", proxyEnvVar)
			fmt.Printf("unset %v\n", clusterEnvVar)
			fmt.Printf("unset %v\n", kubeClusterEnvVar)
			fmt.Printf("unset %v\n", teleport.EnvKubeConfig)
		case !cf.unsetEnvironment:
			kubeName, _ := kubeconfig.SelectedKubeCluster("", profile.Cluster)
			fmt.Printf("export %v=%v\n", proxyEnvVar, profile.ProxyURL.Host)
			fmt.Printf("export %v=%v\n", clusterEnvVar, profile.Cluster)
			if kubeName != "" {
				fmt.Printf("export %v=%v\n", kubeClusterEnvVar, kubeName)
				fmt.Printf("# set %v to a standalone kubeconfig for the selected kube cluster\n", teleport.EnvKubeConfig)
				fmt.Printf("export %v=%v\n", teleport.EnvKubeConfig, profile.KubeConfigPath(kubeName))
			}
		}
	case teleport.JSON, teleport.YAML:
		out, err := serializeEnvironment(profile, format)
		if err != nil {
			return trace.Wrap(err)
		}
		fmt.Println(out)
	}

	return nil
}

func serializeEnvironment(profile *client.ProfileStatus, format string) (string, error) {
	env := map[string]string{
		proxyEnvVar:   profile.ProxyURL.Host,
		clusterEnvVar: profile.Cluster,
	}
	kubeName, _ := kubeconfig.SelectedKubeCluster("", profile.Cluster)
	if kubeName != "" {
		env[kubeClusterEnvVar] = kubeName
		env[teleport.EnvKubeConfig] = profile.KubeConfigPath(kubeName)
	}
	var out []byte
	var err error
	if format == teleport.JSON {
		out, err = utils.FastMarshalIndent(env, "", "  ")
	} else {
		out, err = yaml.Marshal(env)
	}
	return string(out), trace.Wrap(err)
}

// setEnvFlags sets flags that can be set via environment variables.
func setEnvFlags(cf *CLIConf) {
	// these can only be set with env vars.
	if homeDir := os.Getenv(types.HomeEnvVar); homeDir != "" {
		cf.HomePath = filepath.Clean(homeDir)
	}
	if configPath := os.Getenv(globalTshConfigEnvVar); configPath != "" {
		cf.GlobalTshConfigPath = filepath.Clean(configPath)
	}

	// prioritize CLI input for the rest.
	if cf.SiteName == "" {
		// check cluster env variables in order of priority.
		if clusterName := os.Getenv(clusterEnvVar); clusterName != "" {
			cf.SiteName = clusterName
		} else if clusterName = os.Getenv(siteEnvVar); clusterName != "" {
			cf.SiteName = clusterName
		}
	}

	if cf.KubernetesCluster == "" {
		cf.KubernetesCluster = os.Getenv(kubeClusterEnvVar)
	}
}

func handleUnimplementedError(ctx context.Context, perr error, cf CLIConf) error {
	const (
		errMsgFormat         = "This server does not implement this feature yet. Likely the client version you are using is newer than the server. The server version: %v, the client version: %v. Please upgrade the server."
		unknownServerVersion = "unknown"
	)
	tc, err := makeClient(&cf)
	if err != nil {
		logger.WarnContext(ctx, "Failed to create client", "error", err)
		return trace.WrapWithMessage(perr, errMsgFormat, unknownServerVersion, teleport.Version)
	}
	pr, err := tc.Ping(ctx)
	if err != nil {
		logger.WarnContext(ctx, "Failed to call ping", "error", err)
		return trace.WrapWithMessage(perr, errMsgFormat, unknownServerVersion, teleport.Version)
	}
	return trace.WrapWithMessage(perr, errMsgFormat, pr.ServerVersion, teleport.Version)
}

func validateParticipantMode(mode types.SessionParticipantMode) error {
	switch mode {
	case types.SessionPeerMode, types.SessionObserverMode, types.SessionModeratorMode:
		return nil
	default:
		return trace.BadParameter("invalid participant mode %v", mode)
	}
}

// forEachProfileParallel performs an action for each profile a user is currently logged in to in
// parallel.
func forEachProfileParallel(cf *CLIConf, fn func(ctx context.Context, tc *client.TeleportClient, profile *client.ProfileStatus) error) error {
	group, groupCtx := errgroup.WithContext(cf.Context)
	group.SetLimit(6)

	profiles, err := cf.ListProfiles()
	if err != nil {
		return trace.Wrap(err)
	}

	for _, p := range profiles {
		proxyAddr := p.ProxyURL.Host
		if p.IsExpired(time.Now()) {
			fmt.Fprintf(os.Stderr, "Credentials expired for proxy %q, skipping...\n", proxyAddr)
			continue
		}

		group.Go(func() error {
			tc, err := makeClientForProxy(cf, proxyAddr)
			if err != nil {
				return trace.Wrap(err)
			}
			if err := fn(groupCtx, tc, p); err != nil {
				return trace.Wrap(err)
			}

			return nil
		})
	}

	return trace.Wrap(group.Wait())
}

// updateKubeConfigOnLogin checks if the `--kube-cluster` flag was provided to
// tsh login call and updates the default kubeconfig with its value,
// otherwise does nothing.
func updateKubeConfigOnLogin(cf *CLIConf, tc *client.TeleportClient) error {
	if len(cf.KubernetesCluster) == 0 {
		return nil
	}
	kubeStatus, err := fetchKubeStatus(cf.Context, tc)
	if err != nil {
		return trace.Wrap(err)
	}
	// update the default kubeconfig
	kubeConfigPath := ""
	// do not override the context name
	overrideContextName := ""
	err = updateKubeConfig(cf, tc, kubeConfigPath, overrideContextName, kubeStatus)
	return trace.Wrap(err)
}

// onHeadlessApprove executes 'tsh headless approve' command
func onHeadlessApprove(cf *CLIConf) error {
	tc, err := makeClient(cf)
	if err != nil {
		return trace.Wrap(err)
	}

	tc.Stdin = os.Stdin
	err = client.RetryWithRelogin(cf.Context, tc, func() error {
		return tc.HeadlessApprove(cf.Context, cf.HeadlessAuthenticationID, !cf.headlessSkipConfirm)
	})
	return trace.Wrap(err)
}

// getAccessListsToReview will return access lists that the logged in user needs to review. On error,
// this will return an empty list.
func (cf *CLIConf) getAccessListsToReview(tc *client.TeleportClient) []*accesslist.AccessList {
	clusterClient, err := tc.ConnectToCluster(cf.Context)
	if err != nil {
		logger.DebugContext(cf.Context, "Error connecting to the cluster", "error", err)
		return nil
	}
	defer func() {
		clusterClient.Close()
	}()

	// Get the access lists to review. If the call returns NotImplemented, ignore it, as we may be communicating with an OSS
	// server, which does not support access lists.
	accessListsToReview, err := clusterClient.AuthClient.AccessListClient().GetAccessListsToReview(cf.Context)
	if err != nil && !trace.IsNotImplemented(err) {
		logger.DebugContext(cf.Context, "Error getting access lists to review", "error", err)
	}

	return accessListsToReview
}

var mlockModes = []string{mlockModeNo, mlockModeAuto, mlockModeBestEffort, mlockModeStrict}

const (
	// mlockModeNo disables locking process memory.
	mlockModeNo = "off"
	// mlockModeAuto automatically chooses whether memory locking will be attempted and/or enforced.
	mlockModeAuto = "auto"
	// mlockBestEfforts enables locking process memory, but errors will be ignored and logged.
	mlockModeBestEffort = "best_effort"
	// mlockModeStrict enables locking process memory and enforces it succeeds without errors.
	mlockModeStrict = "strict"

	// mlockFailureMessage is a user readable message for mlock errors and debug logs.
	mlockFailureMessage = "Failed to lock process memory for headless login. " +
		"Memory locking is used to prevent secrets in memory from being swapped to disk. " +
		"Please ensure that memory locking is available on your system and your user has " +
		"locking privileges. This means using a Linux operating system and increasing your " +
		`user's memory locking limit to unlimited if needed. Alternatively, set --mlock=off ` +
		"or TELEPORT_MLOCK_MODE=off to disable it. This is not recommended in production " +
		"environments on shared systems where a memory swap attack is possible.\n" +
		"https://goteleport.com/docs/access-controls/guides/headless/#troubleshooting"
)

// Lock the process memory to prevent rsa keys and certificates in memory from being exposed in a swap.
func tryLockMemory(cf *CLIConf) error {
	if cf.MlockMode == mlockModeAuto {
		if cf.AuthConnector == constants.HeadlessConnector {
			// default to best effort for headless login.
			cf.MlockMode = mlockModeBestEffort
		}
	}

	switch cf.MlockMode {
	case mlockModeNo, mlockModeAuto, "":
		// noop
		return nil
	case mlockModeStrict:
		err := mlock.LockMemory()
		return trace.Wrap(err, mlockFailureMessage)
	case mlockModeBestEffort:
		err := mlock.LockMemory()
		logger.WarnContext(cf.Context, mlockFailureMessage, "error", err)
		return nil
	default:
		return trace.BadParameter("unexpected value for --mlock, expected one of (%v)", strings.Join(mlockModes, ", "))
	}
}

// stringFlagToStrings parses a comma-separated string from a CLIConf flag into
// a slice of strings. It trims whitespace from each value and removes
// duplicates.
func stringFlagToStrings(value string) []string {
	values := strings.Split(value, ",")
	for i := range values {
		values[i] = strings.TrimSpace(values[i])
	}
	return apiutils.Deduplicate(values)
}
