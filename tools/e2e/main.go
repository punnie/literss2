// End-to-end check with a virtual WebAuthn authenticator.
// Usage: CHROME=$(which chromium) go run ./tools/e2e INVITE_URL OUTDIR
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/webauthn"
	"github.com/chromedp/chromedp"
)

func main() {
	invite, out := os.Args[1], os.Args[2]
	base := invite[:strings.Index(invite, "/link/")]
	opts := append(chromedp.DefaultExecAllocatorOptions[:], chromedp.ExecPath(os.Getenv("CHROME")))
	alloc, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancel()
	ctx, cancel := chromedp.NewContext(alloc)
	defer cancel()
	ctx, cancel = context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	shot := func(name string, w, h int64, mobile bool) chromedp.Action {
		return chromedp.ActionFunc(func(ctx context.Context) error {
			if err := emulation.SetDeviceMetricsOverride(w, h, 1, mobile).Do(ctx); err != nil {
				return err
			}
			var buf []byte
			if err := chromedp.FullScreenshot(&buf, 80).Do(ctx); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(out, name+".jpg"), buf, 0o644)
		})
	}
	step := func(name string, a ...chromedp.Action) {
		if err := chromedp.Run(ctx, a...); err != nil {
			log.Fatalf("%s: %v", name, err)
		}
		fmt.Println("ok:", name)
	}

	step("virtual authenticator", webauthn.Enable(), chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := webauthn.AddVirtualAuthenticator(&webauthn.VirtualAuthenticatorOptions{
			Protocol: webauthn.AuthenticatorProtocolCtap2, Transport: webauthn.AuthenticatorTransportInternal,
			HasResidentKey: true, HasUserVerification: true, IsUserVerified: true, AutomaticPresenceSimulation: true,
		}).Do(ctx)
		return err
	}))

	var loc, text string
	step("register via invite",
		chromedp.Navigate(invite),
		chromedp.WaitVisible("#name", chromedp.ByQuery),
		shot("link-mobile", 390, 844, true),
		chromedp.SendKeys("#name", "Laptop", chromedp.ByQuery),
		chromedp.Click("#passkey-register button", chromedp.ByQuery),
		chromedp.WaitVisible("ul.river", chromedp.ByQuery),
		chromedp.Location(&loc),
	)
	fmt.Println("  landed on", loc)

	var href string
	step("river",
		shot("river-desktop", 1440, 900, false),
		shot("river-mobile", 390, 844, true),
		chromedp.AttributeValue("ul.river li a", "href", &href, nil, chromedp.ByQuery),
	)
	step("other river", chromedp.Navigate(base+"/r/dev"), chromedp.WaitVisible("ul.river", chromedp.ByQuery),
		chromedp.Text("ul.river", &text, chromedp.ByQuery))
	fmt.Println("  dev river first lines:", firstLines(text, 3))

	step("article "+href,
		chromedp.Navigate(base+href),
		chromedp.WaitVisible("article .content", chromedp.ByQuery),
		chromedp.Text("article", &text, chromedp.ByQuery),
		shot("article-desktop", 1440, 900, false),
		shot("article-mobile", 390, 844, true),
	)
	fmt.Printf("  article text: %d chars: %q\n", len(text), firstLines(text, 2))

	step("account + device link",
		chromedp.Navigate(base+"/account"),
		chromedp.Click(`form[action="/account/link"] button`, chromedp.ByQuery),
		chromedp.WaitVisible("img.qr", chromedp.ByQuery),
		shot("account-mobile", 390, 844, true),
	)

	step("logout", chromedp.Navigate(base+"/account"),
		chromedp.Click(`form[action="/logout"] button`, chromedp.ByQuery),
		chromedp.WaitVisible("#passkey-login", chromedp.ByQuery))

	step("sign in with passkey",
		chromedp.Click("#passkey-login button", chromedp.ByQuery),
		chromedp.WaitVisible("ul.river", chromedp.ByQuery),
		chromedp.Location(&loc),
	)
	fmt.Println("  landed on", loc)

	step("invite reuse rejected",
		network.ClearBrowserCookies(),
		chromedp.Navigate(invite),
		chromedp.Text("main h1", &text, chromedp.ByQuery),
	)
	fmt.Println("  reused invite shows:", text)
}

func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, " | ")
}
