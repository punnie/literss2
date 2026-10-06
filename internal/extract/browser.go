package extract

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/chromedp/chromedp"

	"literss/internal/config"
)

// Chromium no longer honours --load-extension. Extensions are loaded with the
// DevTools Extensions.loadUnpacked method instead, which is only available
// over --remote-debugging-pipe. chromedp only speaks websockets, so chromium is
// started with both: the pipe for loading extensions, the port for chromedp.

// browserFlags mirror chromedp's DefaultExecAllocatorOptions (after
// Puppeteer), minus --disable-extensions.
var browserFlags = []string{
	"--headless",
	"--no-first-run",
	"--no-default-browser-check",
	"--disable-background-networking",
	"--enable-features=NetworkService,NetworkServiceInProcess",
	"--disable-background-timer-throttling",
	"--disable-backgrounding-occluded-windows",
	"--disable-breakpad",
	"--disable-client-side-phishing-detection",
	"--disable-default-apps",
	"--disable-dev-shm-usage",
	"--disable-features=site-per-process,Translate,BlinkGenPropertyTrees",
	"--disable-hang-monitor",
	"--disable-ipc-flooding-protection",
	"--disable-popup-blocking",
	"--disable-prompt-on-repost",
	"--disable-renderer-backgrounding",
	"--disable-sync",
	"--force-color-profile=srgb",
	"--metrics-recording-only",
	"--safebrowsing-disable-auto-update",
	"--enable-automation",
	"--password-store=basic",
	"--use-mock-keychain",
	"--hide-scrollbars",
	"--mute-audio",
	"--blink-settings=imagesEnabled=false",
	"--window-size=1280,1024",
	"--remote-debugging-port=0",
	"--remote-debugging-pipe",
	"--enable-unsafe-extension-debugging",
}

// browserProcess is a running chromium.
type browserProcess struct {
	cmd        *exec.Cmd
	profile    string
	wsURL      string
	exited     chan struct{}
	extensions []loadedExtension
}

type loadedExtension struct {
	id  string
	dir string // the writable copy
}

// isUBOLite reports whether the extension is uBlock Origin Lite.
func (e loadedExtension) isUBOLite() bool {
	_, err := os.Stat(filepath.Join(e.dir, "js", "mode-manager.js"))
	return err == nil
}

// startBrowser launches chromium, loads the configured extensions and returns
// once its DevTools websocket is available.
func startBrowser(cfg config.Chrome, extDir string) (*browserProcess, error) {
	exts, err := prepareExtensions(cfg.Extensions, extDir)
	if err != nil {
		return nil, err
	}
	profile, err := os.MkdirTemp("", "literss-chromium-")
	if err != nil {
		return nil, err
	}
	path := cfg.Path
	if path == "" {
		path = "chromium"
	}
	args := append([]string{"--user-data-dir=" + profile}, browserFlags...)
	if cfg.NoSandbox {
		args = append(args, "--no-sandbox")
	}
	if cfg.UserAgent != "" {
		args = append(args, "--user-agent="+cfg.UserAgent)
	}
	args = append(args, "about:blank")

	// Chromium reads commands from fd 3 and writes responses to fd 4.
	cmdR, cmdW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	resR, resW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(path, args...)
	cmd.ExtraFiles = []*os.File{cmdR, resW}
	// Own process group so Close can take down every chromium process; die
	// with literss.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		os.RemoveAll(profile)
		return nil, fmt.Errorf("starting chromium: %w", err)
	}
	cmdR.Close()
	resW.Close()
	p := &browserProcess{cmd: cmd, profile: profile, exited: make(chan struct{})}
	go func() {
		cmd.Wait()
		cmdW.Close()
		resR.Close()
		close(p.exited)
	}()

	pipe := &devtoolsPipe{w: cmdW, r: bufio.NewReader(resR)}
	for _, dir := range exts {
		raw, err := pipe.call("Extensions.loadUnpacked", map[string]string{"path": dir})
		var res struct {
			ID string `json:"id"`
		}
		if err == nil {
			err = json.Unmarshal(raw, &res)
		}
		if err != nil {
			p.kill()
			return nil, fmt.Errorf("loading extension %s: %w", dir, err)
		}
		p.extensions = append(p.extensions, loadedExtension{id: res.ID, dir: dir})
	}
	if p.wsURL, err = waitForDevToolsPort(profile, p.exited); err != nil {
		p.kill()
		return nil, err
	}
	return p, nil
}

func (p *browserProcess) kill() {
	if p.cmd.Process != nil {
		syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	}
	select {
	case <-p.exited:
	case <-time.After(5 * time.Second):
	}
	os.RemoveAll(p.profile)
}

// connect attaches chromedp to the running browser.
func (p *browserProcess) connect() (context.Context, context.CancelFunc, error) {
	alloc, cancelAlloc := chromedp.NewRemoteAllocator(context.Background(), p.wsURL)
	browser, cancelBrowser := chromedp.NewContext(alloc)
	cancel := func() { cancelBrowser(); cancelAlloc() }
	if err := chromedp.Run(browser); err != nil {
		cancel()
		return nil, nil, fmt.Errorf("connecting to chromium: %w", err)
	}
	// Losing the process makes the browser context unusable.
	go func() {
		select {
		case <-p.exited:
			cancel()
		case <-browser.Done():
		}
	}()
	return browser, cancel, nil
}

func waitForDevToolsPort(profile string, exited <-chan struct{}) (string, error) {
	deadline := time.Now().Add(30 * time.Second)
	for {
		b, err := os.ReadFile(filepath.Join(profile, "DevToolsActivePort"))
		if lines := strings.Split(string(b), "\n"); err == nil && len(lines) >= 2 && lines[1] != "" {
			return "ws://127.0.0.1:" + strings.TrimSpace(lines[0]) + strings.TrimSpace(lines[1]), nil
		}
		select {
		case <-exited:
			return "", errors.New("chromium exited during startup")
		case <-time.After(100 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return "", errors.New("chromium did not open its DevTools port")
		}
	}
}

// devtoolsPipe speaks the DevTools protocol over --remote-debugging-pipe:
// null-terminated JSON messages.
type devtoolsPipe struct {
	w      io.Writer
	r      *bufio.Reader
	nextID int
}

func (p *devtoolsPipe) call(method string, params any) (json.RawMessage, error) {
	p.nextID++
	id := p.nextID
	msg, err := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	if _, err := p.w.Write(append(msg, 0)); err != nil {
		return nil, err
	}
	type result struct {
		raw json.RawMessage
		err error
	}
	done := make(chan result, 1)
	go func() {
		for {
			line, err := p.r.ReadBytes(0)
			if err != nil {
				done <- result{err: err}
				return
			}
			var resp struct {
				ID     int             `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  *struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.Unmarshal(line[:len(line)-1], &resp) != nil || resp.ID != id {
				continue // an event, or a reply to something else
			}
			if resp.Error != nil {
				done <- result{err: errors.New(resp.Error.Message)}
			} else {
				done <- result{raw: resp.Result}
			}
			return
		}
	}()
	select {
	case r := <-done:
		return r.raw, r.err
	case <-time.After(60 * time.Second):
		return nil, fmt.Errorf("%s: no response from chromium", method)
	}
}

// prepareExtensions copies each extension into a writable directory under
// extDir, since chromium writes indexed filter rules into an unpacked
// extension's directory and Nix store paths are read-only. Copies are keyed by
// source path and reused across restarts; stale ones are removed.
func prepareExtensions(srcs []string, extDir string) ([]string, error) {
	if len(srcs) == 0 {
		return nil, nil
	}
	if err := os.MkdirAll(extDir, 0o750); err != nil {
		return nil, err
	}
	keep := map[string]bool{}
	var out []string
	for _, src := range srcs {
		src, err := filepath.EvalSymlinks(src)
		if err != nil {
			return nil, err
		}
		h := sha256.Sum256([]byte(src))
		name := filepath.Base(src)
		if len(name) > 40 {
			name = name[:40]
		}
		name += "-" + hex.EncodeToString(h[:6])
		dst := filepath.Join(extDir, name)
		keep[name] = true
		out = append(out, dst)
		if _, err := os.Stat(filepath.Join(dst, ".literss-complete")); err == nil {
			continue
		}
		os.RemoveAll(dst)
		if err := copyTree(src, dst); err != nil {
			return nil, fmt.Errorf("copying extension %s: %w", src, err)
		}
		if err := os.WriteFile(filepath.Join(dst, ".literss-complete"), nil, 0o640); err != nil {
			return nil, err
		}
	}
	entries, _ := os.ReadDir(extDir)
	for _, e := range entries {
		if !keep[e.Name()] {
			os.RemoveAll(filepath.Join(extDir, e.Name()))
		}
	}
	return out, nil
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
}
