// Package lantern provides an embeddable client-side web proxy
package lantern

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/getlantern/appdir"
	"github.com/getlantern/autoupdate"
	"github.com/getlantern/bandwidth"
	"github.com/getlantern/flashlight"
	"github.com/getlantern/flashlight/client"
	"github.com/getlantern/flashlight/config"
	"github.com/getlantern/flashlight/logging"
	"github.com/getlantern/golog"
	"github.com/getlantern/netx"
	"github.com/getlantern/protected"
	"github.com/getlantern/uuid"

	proclient "github.com/getlantern/pro-server-client/go-client"
)

var (
	log = golog.LoggerFor("lantern")

	updateServerURL = "https://update.getlantern.org"
	defaultLocale   = `en-US`
	surveyURL       = "https://raw.githubusercontent.com/getlantern/loconf/master/ui.json"

	// compileTimePackageVersion is set at compile-time for production builds
	compileTimePackageVersion string

	// if true, run Lantern against our staging infrastructure
	stagingMode = "false"

	startOnce sync.Once
)

func init() {
	proclient.Configure(stagingMode, compileTimePackageVersion)
}

// SocketProtector is an interface for classes that can protect Android sockets,
// meaning those sockets will not be passed through the VPN.
type SocketProtector interface {
	ProtectConn(fileDescriptor int) error
}

// ProtectConnections allows connections made by Lantern to be protected from
// routing via a VPN. This is useful when running Lantern as a VPN on Android,
// because it keeps Lantern's own connections from being captured by the VPN and
// resulting in an infinite loop.
func ProtectConnections(dnsServer string, protector SocketProtector) {
	p := protected.New(protector.ProtectConn, dnsServer)
	netx.OverrideDial(p.Dial)
	netx.OverrideResolve(p.Resolve)
}

// RemoveOverrides removes the protected tlsdialer overrides
// that allowed connections to bypass the VPN.
func RemoveOverrides() {
	netx.Reset()
}

type SurveyInfo struct {
	Enabled  bool   `json:"enabled"`
	Campaign string `json:"campaign"`
	Url      string `json:"url"`
	Message  string `json:"message"`
	Thanks   string `json:"thanks"`
	Button   string `json:"button"`
}

// StartResult provides information about the started Lantern
type StartResult struct {
	HTTPAddr   string
	SOCKS5Addr string
}

type UserConfig interface {
	ConfigUpdate(bool)
	AfterStart()
	ShowSurvey(string)
	BandwidthUpdate(int, int)
}

type Updater autoupdate.Updater

// Start starts a HTTP and SOCKS proxies at random addresses. It blocks up till
// the given timeout waiting for the proxy to listen, and returns the addresses
// at which it is listening (HTTP, SOCKS). If the proxy doesn't start within the
// given timeout, this method returns an error.
//
// If a Lantern proxy is already running within this process, that proxy is
// reused.
//
// Note - this does not wait for the entire initialization sequence to finish,
// just for the proxy to be listening. Once the proxy is listening, one can
// start to use it, even as it finishes its initialization sequence. However,
// initial activity may be slow, so clients with low read timeouts may
// time out.
func Start(configDir string, locale string, timeoutMillis int, user UserConfig) (*StartResult, error) {

	appdir.SetHomeDir(configDir)

	startOnce.Do(func() {
		go run(configDir, locale, user)
	})

	start := time.Now()
	addr, ok := client.Addr(time.Duration(timeoutMillis) * time.Millisecond)
	if !ok {
		return nil, fmt.Errorf("HTTP Proxy didn't start within given timeout")
	}
	elapsed := time.Now().Sub(start)

	socksAddr, ok := client.Socks5Addr((time.Duration(timeoutMillis) * time.Millisecond) - elapsed)
	if !ok {
		return nil, fmt.Errorf("SOCKS5 Proxy didn't start within given timeout")
	}
	return &StartResult{addr.(string), socksAddr.(string)}, nil
}

// AddLoggingMetadata adds metadata for reporting to cloud logging services
func AddLoggingMetadata(key, value string) {
	logging.SetExtraLogglyInfo(key, value)
}

//userConfig supplies user data for fetching user-specific configuration.
type userConfig struct {
}

func (uc *userConfig) GetToken() string {
	return ""
}

func (uc *userConfig) GetUserID() int64 {
	return 0
}

func run(configDir, locale string, user UserConfig) {
	flags := make(map[string]interface{})
	flags["staging"] = false

	err := os.MkdirAll(configDir, 0755)
	if os.IsExist(err) {
		log.Errorf("Unable to create configDir at %v: %v", configDir, err)
		return
	}

	if err := logging.EnableFileLogging(configDir); err != nil {
		log.Errorf("Unable to enable file logging: %v", err)
		return
	}
	log.Debugf("Writing log messages to %s/lantern.log", configDir)

	staging, err := strconv.ParseBool(stagingMode)
	if err == nil {
		flags["staging"] = staging
	} else {
		log.Errorf("Error parsing boolean flag: %v", err)
	}

	flashlight.Run("127.0.0.1:0", // listen for HTTP on random address
		"127.0.0.1:0", // listen for SOCKS on random address
		configDir,     // place to store lantern configuration
		false,         // don't make config sticky
		func() bool { return true },  // proxy all requests
		make(map[string]interface{}), // no special configuration flags
		func() bool {
			//beforeStart(user)
			return true
		}, // beforeStart()
		func() {
			afterStart(user, locale)
		}, // afterStart()
		func(cfg *config.Global) {
			configUpdate(user, cfg)
		}, // onConfigUpdate
		&userConfig{},
		func(err error) {}, // onError
		base64.StdEncoding.EncodeToString(uuid.NodeID()),
	)
}

func bandwidthUpdates(user UserConfig) {
	go func() {
		for quota := range bandwidth.Updates {

			remaining := 0
			percent := 100
			if quota == nil {
				continue
			}

			allowed := quota.MiBAllowed
			if allowed < 0 || allowed > 50000000 {
				continue
			}

			if quota.MiBUsed >= quota.MiBAllowed {
				percent = 100
				remaining = 0
			} else {
				percent = int(100 * (float64(quota.MiBUsed) / float64(quota.MiBAllowed)))
				remaining = int(quota.MiBAllowed - quota.MiBUsed)
			}

			user.BandwidthUpdate(percent, remaining)
		}
	}()
}

func afterStart(user UserConfig, locale string) {
	bandwidthUpdates(user)
	user.AfterStart()
	url, err := surveyRequest(locale)
	if err == nil && url != "" {
		user.ShowSurvey(url)
	}
}

// handleError logs the given error message
func handleError(err error) {
	log.Error(err)
}

func extractUrl(surveys map[string]*json.RawMessage, locale string) (string, error) {

	var survey SurveyInfo
	var err error
	if val, ok := surveys[locale]; ok {
		err = json.Unmarshal(*val, &survey)
		if err != nil {
			handleError(fmt.Errorf("Error parsing survey: %v", err))
			return "", err
		}
		log.Debugf("Found a survey for locale %s: %s", locale, survey.Url)
		return survey.Url, nil
	} else if locale != defaultLocale {
		log.Debugf("No survey found for %s ; Using default locale: %s", locale, defaultLocale)
		return extractUrl(surveys, defaultLocale)
	}
	return "", nil
}

func surveyRequest(locale string) (string, error) {
	var err error
	var req *http.Request
	var res *http.Response

	var surveyResp map[string]*json.RawMessage

	httpClient := &http.Client{}

	if req, err = http.NewRequest("GET", surveyURL, nil); err != nil {
		handleError(fmt.Errorf("Error fetching survey: %v", err))
		return "", err
	}

	if res, err = httpClient.Do(req); err != nil {
		handleError(fmt.Errorf("Error fetching feed: %v", err))
		return "", err
	}

	defer res.Body.Close()

	contents, err := ioutil.ReadAll(res.Body)
	if err != nil {
		handleError(fmt.Errorf("Error reading survey: %v", err))
		return "", err
	}

	err = json.Unmarshal(contents, &surveyResp)
	if err != nil {
		handleError(fmt.Errorf("Error parsing survey: %v", err))
		return "", err
	}

	if surveyResp["survey"] != nil {
		var surveys map[string]*json.RawMessage
		err = json.Unmarshal(*surveyResp["survey"], &surveys)
		if err != nil {
			handleError(fmt.Errorf("Error parsing survey: %v", err))
			return "", err
		}
		locale = strings.Replace(locale, "_", "-", -1)
		return extractUrl(surveys, locale)
	}
	log.Errorf("Error parsing survey response: missing from map")
	return "", nil
}

func configUpdate(user UserConfig, cfg *config.Global) {
	user.ConfigUpdate(cfg.Client.ShowAds)
}

// CheckForUpdates checks to see if a new version of Lantern is available
func CheckForUpdates(shouldProxy bool) (string, error) {
	return autoupdate.CheckMobileUpdate(shouldProxy, updateServerURL,
		compileTimePackageVersion)
}

// DownloadUpdate downloads the latest APK from the given url to the apkPath
// file destination.
func DownloadUpdate(url, apkPath string, shouldProxy bool, updater Updater) {
	autoupdate.UpdateMobile(shouldProxy, url, apkPath, updater)
}
