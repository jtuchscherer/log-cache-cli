package command

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"text/tabwriter"
	"time"

	"code.cloudfoundry.org/cli/plugin"
	logcache "code.cloudfoundry.org/go-log-cache"
	"code.cloudfoundry.org/go-log-cache/rpc/logcache_v1"
)

type app struct {
	GUID string `json:"guid"`
	Name string `json:"name"`
}

type appsResponse struct {
	Resources []app `json:"resources"`
}

type Tailer func(sourceID string, start, end time.Time) []string

// Meta returns the metadata from Log Cache
func Meta(ctx context.Context, cli plugin.CliConnection, tailer Tailer, args []string, c HTTPClient, log Logger, tableWriter io.Writer) {
	f := flag.NewFlagSet("log-cache", flag.ContinueOnError)
	scope := f.String("scope", "all", "")
	enableNoise := f.Bool("noise", false, "")

	err := f.Parse(args)
	if err != nil {
		log.Fatalf("Could not parse flags: %s", err)
	}

	if len(f.Args()) > 0 {
		log.Fatalf("Invalid arguments, expected 0, got %d.", len(f.Args()))
	}

	*scope = strings.ToLower(*scope)
	if invalidScope(*scope) {
		log.Fatalf("Scope must be 'platform', 'applications' or 'all'.")
	}

	logCacheEndpoint, err := logCacheEndpoint(cli)
	if err != nil {
		log.Fatalf("Could not determine Log Cache endpoint: %s", err)
	}

	if strings.ToLower(os.Getenv("LOG_CACHE_SKIP_AUTH")) != "true" {
		c = &tokenHTTPClient{
			c:        c,
			getToken: cli.AccessToken,
		}
	}

	client := logcache.NewClient(
		logCacheEndpoint,
		logcache.WithHTTPClient(c),
	)

	meta, err := client.Meta(ctx)
	if err != nil {
		log.Fatalf("Failed to read Meta information: %s", err)
	}

	meta = truncate(50, meta)
	lines, err := cli.CliCommandWithoutTerminalOutput(
		"curl",
		"/v3/apps?guids="+sourceIDsFromMeta(meta),
	)
	if err != nil {
		log.Fatalf("Failed to make CAPI request: %s", err)
	}

	var resources appsResponse
	err = json.NewDecoder(strings.NewReader(strings.Join(lines, ""))).Decode(&resources)
	if err != nil {
		log.Fatalf("Could not decode CAPI response: %s", err)
	}

	username, err := cli.Username()
	if err != nil {
		log.Fatalf("Could not get username: %s", err)
	}

	fmt.Fprintf(tableWriter, fmt.Sprintf(
		"Retrieving log cache metadata as %s...\n\n",
		username,
	))

	headerArgs := []interface{}{"Source ID", "App Name", "Count", "Expired", "Cache Duration"}
	headerFormat := "%s\t%s\t%s\t%s\t%s\n"
	tableFormat := "%s\t%s\t%d\t%d\t%s\n"

	if *enableNoise {
		headerArgs = append(headerArgs, "Rate")
		headerFormat = strings.Replace(headerFormat, "\n", "\t%s\n", 1)
		tableFormat = strings.Replace(tableFormat, "\n", "\t%d\n", 1)
	}

	tw := tabwriter.NewWriter(tableWriter, 0, 2, 2, ' ', 0)
	fmt.Fprintf(tw, headerFormat, headerArgs...)

	for _, app := range resources.Resources {
		m := meta[app.GUID]
		delete(meta, app.GUID)
		if *scope == "applications" || *scope == "all" {
			args := []interface{}{app.GUID, app.Name, m.Count, m.Expired, cacheDuration(m)}
			if *enableNoise {
				end := time.Now()
				start := end.Add(-time.Minute)
				args = append(args, len(tailer(app.GUID, start, end)))
			}

			fmt.Fprintf(tw, tableFormat, args...)
		}
	}

	idRegexp := regexp.MustCompile("[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}")

	// Apps that do not have a known name from CAPI
	if *scope == "applications" || *scope == "all" {
		for sourceID, m := range meta {
			if idRegexp.MatchString(sourceID) {
				args := []interface{}{sourceID, "", m.Count, m.Expired, cacheDuration(m)}
				if *enableNoise {
					end := time.Now()
					start := end.Add(-time.Minute)
					args = append(args, len(tailer(sourceID, start, end)))
				}
				fmt.Fprintf(tw, tableFormat, args...)
			}
		}
	}

	if *scope == "platform" || *scope == "all" {
		for sourceID, m := range meta {
			if !idRegexp.MatchString(sourceID) {
				args := []interface{}{sourceID, "", m.Count, m.Expired, cacheDuration(m)}
				if *enableNoise {
					end := time.Now()
					start := end.Add(-time.Minute)
					args = append(args, len(tailer(sourceID, start, end)))
				}

				fmt.Fprintf(tw, tableFormat, args...)
			}
		}
	}

	tw.Flush()
}

func cacheDuration(m *logcache_v1.MetaInfo) time.Duration {
	new := time.Unix(0, m.NewestTimestamp)
	old := time.Unix(0, m.OldestTimestamp)
	return new.Sub(old).Truncate(time.Second)
}

func truncate(count int, entries map[string]*logcache_v1.MetaInfo) map[string]*logcache_v1.MetaInfo {
	truncated := make(map[string]*logcache_v1.MetaInfo)
	for k, v := range entries {
		if len(truncated) >= count {
			break
		}
		truncated[k] = v
	}
	return truncated
}

func logCacheEndpoint(cli plugin.CliConnection) (string, error) {
	logCacheAddr := os.Getenv("LOG_CACHE_ADDR")

	if logCacheAddr != "" {
		return logCacheAddr, nil
	}

	apiEndpoint, err := cli.ApiEndpoint()
	if err != nil {
		return "", err
	}

	return strings.Replace(apiEndpoint, "api", "log-cache", 1), nil
}

func sourceIDsFromMeta(meta map[string]*logcache_v1.MetaInfo) string {
	var ids []string
	for id := range meta {
		ids = append(ids, id)
	}
	return strings.Join(ids, ",")
}

func invalidScope(scope string) bool {
	validScopes := []string{"platform", "applications", "all"}

	if scope == "" {
		return false
	}

	for _, s := range validScopes {
		if scope == s {
			return false
		}
	}

	return true
}
