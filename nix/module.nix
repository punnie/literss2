self:
{ config, lib, pkgs, ... }:

let
  cfg = config.services.literss;
  toml = pkgs.formats.toml { };
  configFile = toml.generate "literss.toml" cfg.settings;

  # Admin CLI that runs as the service user against the service's config,
  # e.g. `sudo literss invite alice`.
  cli = pkgs.writeShellScriptBin "literss" ''
    exec ${pkgs.util-linux}/bin/runuser -u ${cfg.user} -- \
      ${lib.getExe cfg.package} -config ${configFile} "$@"
  '';
in
{
  options.services.literss = {
    enable = lib.mkEnableOption "literss, a minimalist river-of-news reader backed by Miniflux";

    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.literss;
      defaultText = lib.literalExpression "literss.packages.\${system}.literss";
    };

    chromium = lib.mkOption {
      type = lib.types.package;
      default = pkgs.chromium;
      defaultText = lib.literalExpression "pkgs.chromium";
      description = "Chromium used by feeds with the `chrome` extractor.";
    };

    ublock = {
      enable = lib.mkOption {
        type = lib.types.bool;
        default = true;
        description = "Load uBlock Origin Lite into Chromium so ads are neither rendered nor extracted.";
      };
      package = lib.mkOption {
        type = lib.types.package;
        default = self.packages.${pkgs.stdenv.hostPlatform.system}.ubol;
        defaultText = lib.literalExpression "literss.packages.\${system}.ubol";
      };
      mode = lib.mkOption {
        type = lib.types.enum [ "basic" "optimal" "complete" ];
        default = "complete";
        description = ''
          uBO Lite filtering mode. "complete" adds generic cosmetic filters,
          whose hidden elements are removed before extraction (see the per-feed
          `chrome.keep_hidden`).
        '';
      };
    };

    domain = lib.mkOption {
      type = lib.types.str;
      default = "literss.ethereal.io";
      description = "Public host name. It is also the passkey relying party ID, so changing it invalidates existing passkeys.";
    };

    port = lib.mkOption {
      type = lib.types.port;
      default = 8090;
      description = "Port to listen on, on localhost.";
    };

    user = lib.mkOption {
      type = lib.types.str;
      default = "literss";
    };

    nginx = {
      enable = lib.mkEnableOption "an nginx virtual host with ACME for literss";
    };

    settings = lib.mkOption {
      description = ''
        literss configuration, written to a TOML file. See config.example.toml.
        Use `*_file` options (miniflux.token_file, feed cookies_file and
        password_file) for secrets; those files must be readable by the
        literss user.
      '';
      default = { };
      type = lib.types.submodule {
        freeformType = toml.type;
        options = {
          listen = lib.mkOption {
            type = lib.types.str;
            default = "127.0.0.1:${toString cfg.port}";
            defaultText = lib.literalExpression ''"127.0.0.1:''${toString config.services.literss.port}"'';
          };
          base_url = lib.mkOption {
            type = lib.types.str;
            default = "https://${cfg.domain}";
            defaultText = lib.literalExpression ''"https://''${config.services.literss.domain}"'';
          };
          data_dir = lib.mkOption {
            type = lib.types.str;
            default = "/var/lib/literss";
          };
          time_zone = lib.mkOption {
            type = lib.types.str;
            default = if config.time.timeZone != null then config.time.timeZone else "UTC";
            defaultText = lib.literalExpression "config.time.timeZone or \"UTC\"";
          };
          session_ttl = lib.mkOption {
            type = lib.types.str;
            default = "168h";
          };
          prefetch_interval = lib.mkOption {
            type = lib.types.str;
            default = "15m";
            description = "How often the newest entries of each river are extracted ahead of time. Empty disables.";
          };
          miniflux = {
            url = lib.mkOption {
              type = lib.types.str;
              example = "http://127.0.0.1:8080";
            };
            token_file = lib.mkOption {
              type = lib.types.path;
              description = "File containing a Miniflux API token.";
            };
          };
          chrome = lib.mkOption {
            type = lib.types.submodule {
              freeformType = toml.type;
              options = {
                path = lib.mkOption {
                  type = lib.types.str;
                  default = lib.getExe cfg.chromium;
                  defaultText = lib.literalExpression "lib.getExe config.services.literss.chromium";
                };
                extensions = lib.mkOption {
                  type = lib.types.listOf lib.types.str;
                  default = lib.optional cfg.ublock.enable "${cfg.ublock.package}";
                  defaultText = lib.literalExpression ''lib.optional config.services.literss.ublock.enable "''${config.services.literss.ublock.package}"'';
                  description = "Unpacked Chromium extensions to load.";
                };
                ublock_mode = lib.mkOption {
                  type = lib.types.str;
                  default = lib.optionalString cfg.ublock.enable cfg.ublock.mode;
                  defaultText = lib.literalExpression "config.services.literss.ublock.mode";
                };
              };
            };
            default = { };
          };
          rivers = lib.mkOption {
            type = lib.types.listOf toml.type;
            description = "Rivers in display order; the first one is the home page.";
          };
        };
      };
    };
  };

  config = lib.mkIf cfg.enable {
    users.users.${cfg.user} = {
      isSystemUser = true;
      group = cfg.user;
      home = cfg.settings.data_dir;
    };
    users.groups.${cfg.user} = { };

    environment.systemPackages = [ cli ];

    systemd.services.literss = {
      description = "literss river reader";
      wantedBy = [ "multi-user.target" ];
      wants = [ "network-online.target" ];
      after = [ "network-online.target" ] ++ lib.optional config.services.miniflux.enable "miniflux.service";
      restartTriggers = [ configFile ];
      environment = {
        HOME = cfg.settings.data_dir;
        XDG_CONFIG_HOME = "${cfg.settings.data_dir}/.config";
        XDG_CACHE_HOME = "${cfg.settings.data_dir}/.cache";
      };
      serviceConfig = {
        ExecStart = "${lib.getExe cfg.package} -config ${configFile} serve";
        User = cfg.user;
        Group = cfg.user;
        StateDirectory = lib.mkIf (cfg.settings.data_dir == "/var/lib/literss") "literss";
        StateDirectoryMode = "0750";
        WorkingDirectory = cfg.settings.data_dir;
        Restart = "on-failure";
        RestartSec = 5;

        # Hardening. Chromium needs user namespaces for its own sandbox and a
        # writable+executable heap for V8, so RestrictNamespaces and
        # MemoryDenyWriteExecute are deliberately not set.
        NoNewPrivileges = true;
        CapabilityBoundingSet = "";
        PrivateTmp = true;
        PrivateDevices = true;
        ProtectSystem = "strict";
        ProtectHome = true;
        ProtectClock = true;
        ProtectHostname = true;
        ProtectKernelLogs = true;
        ProtectKernelModules = true;
        ProtectKernelTunables = true;
        ProtectControlGroups = true;
        ProtectProc = "invisible";
        LockPersonality = true;
        RestrictRealtime = true;
        RestrictSUIDSGID = true;
        RestrictAddressFamilies = [ "AF_UNIX" "AF_INET" "AF_INET6" "AF_NETLINK" ];
        UMask = "0027";
      };
    };

    services.nginx = lib.mkIf cfg.nginx.enable {
      enable = true;
      recommendedProxySettings = true;
      recommendedTlsSettings = true;
      recommendedGzipSettings = true;
      virtualHosts.${cfg.domain} = {
        forceSSL = true;
        enableACME = true;
        locations."/".proxyPass = "http://${cfg.settings.listen}";
      };
    };
  };
}
