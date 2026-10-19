package gitcache

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	maxStderr    = 4 << 10
	gitWaitDelay = 5 * time.Second
)

// gitConfig applies to every git process the cache runs.
var gitConfig = [][2]string{
	// git would follow a redirect from the relay straight to the network.
	{"http.followRedirects", "false"},
	{"protocol.version", "2"},
	// Auto gc stays on, as on any Git server, but runs inside the fetch
	// rather than detaching, so no git process outlives the fetch that
	// started it, and sizes are measured after it.
	{"gc.autoDetach", "false"},
	{"maintenance.autoDetach", "false"},
	{"init.defaultBranch", "main"},
	// Keep every fetched pack as a pack. index-pack writes it to a
	// temporary file and moves it into place only once complete, whereas a
	// small pack is otherwise unpacked object by object, and a transfer cut
	// off at max_repo_size would leave the objects before the cut, such as a
	// commit without its tree's blobs.
	{"transfer.unpackLimit", "1"},
	// Serve any commit the cache holds when a client asks for it by object
	// ID, as the upstream would, including ones a force-push left behind,
	// and do not advertise the refs that keep fetched ones.
	{"uploadpack.allowAnySHA1InWant", "true"},
	{"uploadpack.hideRefs", wantedPrefix},
	// With http.lowSpeedTime, added per cache, gives up on a transfer that
	// receives nothing for that long, so a connection that stalls mid-pack
	// cannot hold a refresh for fetchTimeout.
	{"http.lowSpeedLimit", "1"},
	// index-pack streams blobs larger than this instead of holding them in
	// memory, and pack-objects stores them without delta compression, so a
	// large file costs a fetch or a clone little memory.
	{"core.bigFileThreshold", "8m"},
}

// command builds a git process with an environment of its own: none of the
// proxy's environment is passed on, so no system or user config, credential
// helper, HTTP proxy setting or GIT_ variable applies, and the only transport
// git may use is plain HTTP, which is what the relay speaks.
func (c *Cache) command(ctx context.Context, extraEnv []string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, c.gitPath, args...)
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + c.tmpDir(),
		"LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ALLOW_PROTOCOL=http",
	}
	config := slices.Concat(gitConfig, [][2]string{{"http.lowSpeedTime", strconv.Itoa(c.lowSpeedTime)}})
	env = append(env, "GIT_CONFIG_COUNT="+strconv.Itoa(len(config)))
	for i, kv := range config {
		env = append(env,
			"GIT_CONFIG_KEY_"+strconv.Itoa(i)+"="+kv[0],
			"GIT_CONFIG_VALUE_"+strconv.Itoa(i)+"="+kv[1])
	}
	env = append(env, extraEnv...)
	cmd.Env = env
	cmd.WaitDelay = gitWaitDelay
	return cmd
}

// run runs git in the repository at dir and returns its standard output.
func (c *Cache) run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := c.command(ctx, nil, append([]string{"-C", dir}, args...)...)
	var stdout bytes.Buffer
	stderr := &limitedBuffer{max: maxStderr}
	cmd.Stdout = &stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// limitedBuffer keeps the first max bytes written to it.
type limitedBuffer struct {
	bytes.Buffer
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.Len(); room > 0 {
		b.Buffer.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}
