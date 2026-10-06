# uBlock Origin Lite, unpacked, for loading into headless Chromium.
{ fetchzip }:

fetchzip {
  pname = "ubol";
  version = "2026.930.1227";
  url = "https://github.com/uBlockOrigin/uBOL-home/releases/download/2026.930.1227/uBOLite_2026.930.1227.chromium.zip";
  stripRoot = false;
  hash = "sha256-RaCPzuREHDZqOGdnd1Ptuqg4oMqA0PNPTVrsZDtlWz4=";
}
