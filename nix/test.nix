# NixOS VM test: Miniflux + literss + a local feed site.
self:
{ pkgs, lib, ... }:

let
  para = "This is a long paragraph of article text so that readability considers it the main content of the page. ";
  article = marker: ''
    <!doctype html><html><head><title>${marker}</title></head><body>
    <nav><a href="/">Home</a> <a href="/about">About</a></nav>
    <article><h1>${marker}</h1>
    <p>${lib.strings.replicate 6 para}</p>
    <p id="marker">${marker} body text, unique to this article.</p>
    <p>${lib.strings.replicate 6 para}</p>
    </article>
    <footer>Copyright</footer>
    <script>document.getElementById("marker").insertAdjacentHTML("afterend", "<p>Rendered by JavaScript.</p>")</script>
    </body></html>
  '';
  feed = name: marker: ''
    <?xml version="1.0"?>
    <rss version="2.0"><channel><title>${name}</title><link>http://feeds.test/</link><description>test</description>
    <item><title>${marker} title</title><link>http://feeds.test/${marker}.html</link><guid>${marker}</guid>
    <pubDate>Mon, 05 Oct 2026 10:00:00 GMT</pubDate><description>Summary of ${marker}.</description></item>
    </channel></rss>
  '';
  # An ad-supported page: a script from an ad server (uBO Lite blocks it), a
  # box hidden by a generic cosmetic filter, and a paragraph that only appears
  # once the bottom of the page scrolls into view.
  adArticle = ''
    <!doctype html><html><head><title>adarticle</title></head><body>
    <article><h1>adarticle</h1>
    <p>${lib.strings.replicate 6 para}</p>
    <div id="AdBillboard">SPONSORED-BOX buy things now, this sponsored box has a lot of words in it.</div>
    <p id="net">Ad script marker paragraph, still pending, with enough text to be kept as content.</p>
    <script src="http://acdn.adnxs.com/ast/ast.js"
      onload="document.getElementById('net').textContent = 'AD-SCRIPT-RAN the ad script loaded and ran, which it should not.'"
      onerror="document.getElementById('net').textContent = 'AD-SCRIPT-BLOCKED the ad script was blocked from loading, as it should be.'"></script>
    <p>${lib.strings.replicate 6 para}</p>
    <div style="height: 4000px"></div>
    <p id="sentinel">End of page paragraph with enough words in it to be kept by readability as content.</p>
    </article>
    <script>
    new IntersectionObserver((entries, o) => {
      if (entries.some(e => e.isIntersecting)) {
        document.getElementById("sentinel").insertAdjacentHTML("afterend", "<p>LAZY-LOADED paragraph that appears once the end of the page is scrolled into view.</p>");
        o.disconnect();
      }
    }).observe(document.getElementById("sentinel"));
    </script>
    </body></html>
  '';
  adServer = pkgs.runCommand "ad-server" { } ''
    mkdir -p $out/ast
    echo 'window.adScriptLoaded = true;' > $out/ast/ast.js
  '';
  site = pkgs.runCommand "feed-site" { } ''
    mkdir $out
    cp ${pkgs.writeText "a" (feed "Chrome Feed" "chromearticle")} $out/chrome.xml
    cp ${pkgs.writeText "b" (feed "Miniflux Feed" "minifluxarticle")} $out/miniflux.xml
    cp ${pkgs.writeText "c" (article "chromearticle")} $out/chromearticle.html
    cp ${pkgs.writeText "d" (article "minifluxarticle")} $out/minifluxarticle.html
    cp ${pkgs.writeText "e" (feed "Ad Feed" "adarticle")} $out/ads.xml
    cp ${pkgs.writeText "f" adArticle} $out/adarticle.html
  '';
in
pkgs.testers.runNixOSTest {
  name = "literss";
  nodes.machine = { config, ... }: {
    imports = [ self.nixosModules.default ];
    virtualisation.memorySize = 3072;
    virtualisation.cores = 2;
    environment.systemPackages = [ pkgs.sqlite pkgs.jq ];
    networking.hosts."127.0.0.1" = [ "feeds.test" "acdn.adnxs.com" ];

    services.nginx = {
      enable = true;
      virtualHosts."feeds.test".root = site;
      virtualHosts."acdn.adnxs.com".root = adServer;
    };

    services.miniflux = {
      enable = true;
      adminCredentialsFile = pkgs.writeText "miniflux-admin" ''
        ADMIN_USERNAME=admin
        ADMIN_PASSWORD=correct-horse
      '';
      config.LISTEN_ADDR = "127.0.0.1:8080";
      # The test feeds are served from localhost.
      config.FETCHER_ALLOW_PRIVATE_NETWORKS = "1";
    };

    services.literss = {
      enable = true;
      settings = {
        miniflux.url = "http://127.0.0.1:8080";
        miniflux.token_file = "/run/literss-token";
        prefetch_interval = "";
        rivers = [
          {
            slug = "news";
            title = "News";
            feeds = [
              { url = "http://feeds.test/chrome.xml"; extractor = "chrome"; }
              { url = "http://feeds.test/miniflux.xml"; extractor = "miniflux"; }
              {
                url = "http://feeds.test/ads.xml";
                extractor = "chrome";
                chrome.steps = [ { scroll = "bottom"; } ];
              }
            ];
          }
          { slug = "empty"; title = "Empty"; feeds = [ ]; }
        ];
      };
    };
    # Started by the test once a Miniflux token exists.
    systemd.services.literss.wantedBy = lib.mkForce [ ];
  };

  testScript = ''
    import re

    machine.wait_for_unit("miniflux.service")
    machine.wait_for_unit("nginx.service")
    machine.wait_for_open_port(8080)

    machine.succeed(
      "curl -sf -u admin:correct-horse -H 'Content-Type: application/json' "
      "-d '{\"description\":\"literss\"}' http://127.0.0.1:8080/v1/api-keys "
      "| jq -r .token > /run/literss-token && chown literss /run/literss-token"
    )
    machine.succeed("systemctl start literss")
    machine.wait_for_open_port(8090)
    machine.wait_until_succeeds("journalctl -u literss | grep -q 'miniflux sync complete'", timeout=60)

    with subtest("admin CLI creates invite links"):
      link = machine.succeed("literss invite alice 2>/dev/null").strip()
      assert re.fullmatch(r"https://literss\.ethereal\.io/link/[A-Za-z0-9_-]{43}", link), link
      token = link.rsplit("/", 1)[1]
      machine.succeed(f"curl -sf http://127.0.0.1:8090/link/{token} | grep -q 'Create passkey'")
      assert "alice" in machine.succeed("literss users")

    with subtest("unauthenticated requests go to the login page"):
      machine.succeed("curl -s -o /dev/null -w '%{redirect_url}' http://127.0.0.1:8090/ | grep -q /login")

    # Fake a session for the HTTP checks; passkeys are covered by dev/e2e.
    machine.succeed(
      "runuser -u literss -- sqlite3 /var/lib/literss/literss.db "
      "\"INSERT INTO sessions VALUES (X'$(printf testtoken | sha256sum | cut -d' ' -f1)', 1, strftime('%s'), strftime('%s') + 3600)\""
    )
    get = "curl -sf -H 'Cookie: literss_session=testtoken' http://127.0.0.1:8090"

    with subtest("river lists entries from all feeds"):
      machine.wait_until_succeeds(
        "curl -sf -u admin:correct-horse -X PUT http://127.0.0.1:8080/v1/feeds/refresh; "
        + get + "/ | grep -q chromearticle && " + get + "/ | grep -q minifluxarticle && " + get + "/ | grep -q adarticle",
        timeout=120,
      )
      river = machine.succeed(get + "/")
      assert "Chrome Feed" in river and "Miniflux Feed" in river, river

    def article(marker):
      m = re.search(r'href="(/a/\d+)">' + marker, river)
      assert m, marker
      return machine.succeed(get + m.group(1))

    with subtest("chrome extractor renders and extracts under the service sandbox"):
      page = article("chromearticle")
      assert "unique to this article" in page, page
      assert "Rendered by JavaScript" in page, page
      assert "Copyright" not in page

    with subtest("uBlock Origin Lite blocks ads; steps scroll to the bottom"):
      page = article("adarticle")
      assert "AD-SCRIPT-BLOCKED" in page, page
      assert "AD-SCRIPT-RAN" not in page, page
      assert "SPONSORED-BOX" not in page, page
      assert "LAZY-LOADED" in page, page
      machine.succeed("journalctl -u literss | grep -q 'chromium started.*extensions=1 ublock_mode=complete'")

    with subtest("miniflux extractor"):
      page = article("minifluxarticle")
      assert "unique to this article" in page, page

    with subtest("config is reconciled idempotently on restart"):
      machine.succeed("systemctl restart literss")
      machine.wait_until_succeeds("journalctl -u literss --since '-30s' | grep -q 'miniflux sync complete'", timeout=60)
      feeds = machine.succeed("curl -sf -H \"X-Auth-Token: $(cat /run/literss-token)\" http://127.0.0.1:8080/v1/feeds | jq length").strip()
      assert feeds == "3", feeds
  '';
}
