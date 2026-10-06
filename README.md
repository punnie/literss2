# literss

A minimalist, invite-only river-of-news reader in the style of
[lite.cnn.com](https://lite.cnn.com): each river is a plain list of titles,
newest first, and clicking one shows the article's readable text and images.

[Miniflux](https://miniflux.app) does all feed fetching and storage. literss
reads from it, and extracts articles either with Miniflux's
[fetch-content](https://miniflux.app/docs/api.html#endpoint-fetch-content)
endpoint or by rendering the page in headless Chromium and running readability
on the result. Extracted articles and their images are cached locally, so pages
never hotlink.

## How it works

- **Rivers** are defined in the config. Each river is a Miniflux category with
  the same title, and each feed is a Miniflux feed in that category. On startup
  (and every `sync_interval`) literss creates missing categories and feeds and
  pushes the configured feed settings. Feeds that aren't in the config are left
  alone, but are never shown. A feed can belong to only one river. Use a
  dedicated Miniflux user for literss.
- **The first river** is the home page.
- **Extractors** are set per feed:
  - `miniflux` (default): Miniflux's fetch-content. Uses the feed's
    `scraper_rules`, `rewrite_rules`, `user_agent`, `username` and `password`,
    which are also pushed to Miniflux for polling the feed.
  - `chrome`: headless Chromium plus readability, with optional
    `chrome.wait_selector`, `chrome.wait` and `chrome.user_agent`.
- **Cookies** for private feeds go in a per-feed `cookies_file`: a Netscape
  `cookies.txt` or a browser-extension JSON export. The same cookies are sent
  by Miniflux (polling and fetch-content), injected into Chromium, and used
  when caching that feed's images. Expired cookies are skipped.
- **Prefetching**: every `prefetch_interval`, the newest `items_per_page`
  entries of each river are extracted ahead of time. Failures are retried after
  6 hours. If extraction fails when someone opens an article, the feed's own
  summary is shown instead and nothing is cached.
- **Images** in articles are rewritten to `/img/<id>` and downloaded into
  `data_dir/images`. Only public addresses are fetched.

## Accounts

There are no passwords, email or phone. An admin creates a single-use link:

```sh
sudo literss invite alice        # on NixOS (runs as the service user)
literss -config config.toml invite alice
```

The link is valid for 24 hours. Opening it lets the person create a passkey,
which signs them in. From **Account** they can create a 15-minute single-use
link (with a QR code) to add a passkey on another device, remove passkeys
(never the last one), and sign out on this device or everywhere. Sessions last
7 days and are extended on each use.

Other commands: `literss users`, `literss delete-user NAME`, `literss sync`.

Passkeys are tied to the domain in `base_url` (`literss.ethereal.io`), so
changing it invalidates every passkey.

## NixOS

```nix
{
  inputs.literss.url = "git+https://…/literss";

  outputs = { nixpkgs, literss, ... }: {
    nixosConfigurations.host = nixpkgs.lib.nixosSystem {
      modules = [
        literss.nixosModules.default
        {
          services.literss = {
            enable = true;
            domain = "literss.ethereal.io";   # the default
            nginx.enable = true;              # vhost + ACME, optional
            settings = {
              miniflux.url = "http://127.0.0.1:8080";
              miniflux.token_file = "/run/agenix/literss-miniflux-token";
              rivers = [
                {
                  slug = "news";
                  title = "News";
                  feeds = [
                    { url = "https://feeds.arstechnica.com/arstechnica/index"; extractor = "miniflux"; }
                    { url = "https://www.theverge.com/rss/index.xml"; extractor = "chrome"; }
                  ];
                }
              ];
            };
          };
        }
      ];
    };
  };
}
```

`settings` maps one-to-one onto the TOML config (see `config.example.toml`).
Keep secrets in `*_file` options. Those files must be readable by the
`literss` user. The module sets `chrome.path` to nixpkgs' Chromium and runs
the service hardened, while keeping Chromium's own sandbox working.

## Development

```sh
nix develop                                    # go, gopls, sqlite, chromium
go test ./...
go run . -config dev/config.toml serve
go run . -config dev/config.toml invite me     # http://localhost works for passkeys
nix build                                      # package
nix build .#checks.x86_64-linux.vm -L          # NixOS VM test: miniflux + literss + both extractors
```

After changing Go dependencies, update `vendorHash` in `nix/package.nix`.
`tools/e2e` drives Chromium with a virtual WebAuthn authenticator
through the full invite → passkey → browse → sign-in flow:
`CHROME=$(which chromium) go run ./tools/e2e "$(go run . -config dev/config.toml invite me)" /tmp/shots`.
