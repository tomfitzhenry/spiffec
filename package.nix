{ lib, buildGoModule }:

buildGoModule {
  pname = "spiffec";
  version = "0.1.0";
  src = ./.;

  vendorHash = "sha256-i0CuA5iNHtNfH2R96Z1+Wa5D4qZAfA1SeRB/YyhE5wI=";

  meta = {
    description = "Per-connection stdio to SPIFFE mTLS client tunnel";
    homepage = "https://github.com/tomfitzhenry/spiffec";
    license = lib.licenses.mit;
    mainProgram = "spiffec";
    platforms = lib.platforms.linux;
  };
}
