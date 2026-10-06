{ lib, buildGoModule }:

buildGoModule {
  pname = "literss";
  version = "0.1.0";

  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [
      ../go.mod
      ../go.sum
      ../main.go
      ../internal
    ];
  };

  vendorHash = "sha256-LkIEhqnm+5p12Kgxr9/ocgwSdZwZ1titGx4RBbGe29k=";

  env.CGO_ENABLED = 0;
  ldflags = [ "-s" "-w" ];

  meta = {
    description = "Minimalist river-of-news reader backed by Miniflux";
    mainProgram = "literss";
    platforms = lib.platforms.linux;
  };
}
