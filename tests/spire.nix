# End-to-end NixOS VM test for spiffec.
#
# One VM runs SPIRE server + agent (x509pop node attestation, unix workload
# attestation), sshd on 127.0.0.1:22, and ghostunnel server on 127.0.0.1:2222
# forwarding to sshd and allowing only spiffe://example.org/client/alice.
{ pkgs, spiffec }:

let
  trustDomain = "example.org";

  aliceUid = 4500;
  malloryUid = 4501;
  eveUid = 4502;

  spireServerSocket = "/run/spire/server/private/api.sock";
  spireAgentSocket = "/run/spire/agent/public/api.sock";
  agentBundle = "/run/spire/agent/bundle.pem";

  nodeID = "spiffe://${trustDomain}/agent/spire-node";
  serverID = "spiffe://${trustDomain}/server/frontdoor";
  aliceID = "spiffe://${trustDomain}/client/alice";
  malloryID = "spiffe://${trustDomain}/client/mallory";

  frontdoorListen = "127.0.0.1:2222";

  # nixpkgs 26.05 packages ghostunnel 1.8.4, which predates
  # https://github.com/ghostunnel/ghostunnel/commit/2fcd777d47c2d00827af7567bbe280a032dd471e
  # ("Half-close TLS connections during copy teardown"): on a client TLS
  # close_notify, 1.8.4 hard-closes the whole *tls.Conn instead of half-closing
  # the backend, dropping in-flight return traffic. Build ghostunnel from HEAD.
  ghostunnel = pkgs.buildGoModule {
    pname = "ghostunnel";
    version = "1.11.5-unstable-2026-11-08";
    src = pkgs.fetchFromGitHub {
      owner = "ghostunnel";
      repo = "ghostunnel";
      rev = "d8ceb991eafb2555c37d9757e1deaf0b6e7563cd";
      hash = "sha256-qhVQC/2/KpN1L1OnnSCkouYp9uD2b+kn9eyFLSbn1ck=";
    };
    vendorHash = null;
    postPatch = ''
      sed -i 's/^go 1\.27.*/go 1.26.0/' go.mod
      sed -i 's/go 1\.27\.[0-9]*/go 1.26.0/g' vendor/modules.txt
    '';
    doCheck = false;
  };

  testCerts = pkgs.runCommand "spiffec-test-certs" {
    nativeBuildInputs = [
      pkgs.openssl
      pkgs.openssh
    ];
  } ''
    mkdir -p $out
    cd $out

    openssl ecparam -name prime256v1 -genkey -noout -out ca.key
    openssl req -x509 -new -key ca.key -sha256 -days 3650 \
      -subj "/CN=spiffec-test-ca" \
      -addext "basicConstraints=critical,CA:TRUE" \
      -addext "keyUsage=critical,keyCertSign,cRLSign" -out ca.crt

    printf '%s\n' 'keyUsage=digitalSignature' 'extendedKeyUsage=clientAuth' > agent.ext
    openssl ecparam -name prime256v1 -genkey -noout -out agent.key
    openssl req -new -key agent.key -subj "/CN=spire-test-agent" -out agent.csr
    openssl x509 -req -in agent.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
      -days 3650 -sha256 -extfile agent.ext -out agent.crt

    ssh-keygen -t ed25519 -N "" -f alice_id_ed25519 -q
  '';

  bootstrap = pkgs.writeShellScript "spire-bootstrap" ''
    set -eu
    server=${pkgs.spire}/bin/spire-server
    socket=${spireServerSocket}

    for _ in $(${pkgs.coreutils}/bin/seq 1 300); do
      if "$server" healthcheck -socketPath "$socket" >/dev/null 2>&1; then
        break
      fi
      ${pkgs.coreutils}/bin/sleep 1
    done

    ${pkgs.coreutils}/bin/install -d -m 0755 -o spire-agent -g spire-agent /run/spire/agent
    "$server" bundle show -socketPath "$socket" -format pem > ${agentBundle}
    ${pkgs.coreutils}/bin/chown spire-agent:spire-agent ${agentBundle}
    ${pkgs.coreutils}/bin/chmod 0444 ${agentBundle}

    have_entry() {
      "$server" entry show -socketPath "$socket" -output json | ${pkgs.gnugrep}/bin/grep -q "$1"
    }

    if ! have_entry "${nodeID}"; then
      "$server" entry create -socketPath "$socket" -node \
        -parentID "spiffe://${trustDomain}/spire/server" \
        -spiffeID "${nodeID}" \
        -selector "x509pop:subject:cn:spire-test-agent"
    fi

    create_workload() {
      if ! have_entry "$2"; then
        "$server" entry create -socketPath "$socket" -parentID "${nodeID}" \
          -spiffeID "$2" -selector "$1"
      fi
    }

    create_workload "unix:user:frontdoor" "${serverID}"
    create_workload "unix:uid:${toString aliceUid}" "${aliceID}"
    create_workload "unix:uid:${toString malloryUid}" "${malloryID}"
  '';
in
pkgs.testers.nixosTest {
  name = "spiffec";

  nodes.machine =
    { ... }:
    {
      virtualisation = {
        memorySize = 1536;
        cores = 2;
      };
      system.stateVersion = "26.05";

      users.users = {
        frontdoor = {
          isSystemUser = true;
          group = "frontdoor";
        };
        alice = {
          isNormalUser = true;
          uid = aliceUid;
          group = "alice";
          openssh.authorizedKeys.keyFiles = [ "${testCerts}/alice_id_ed25519.pub" ];
        };
        mallory = {
          isNormalUser = true;
          uid = malloryUid;
          group = "mallory";
        };
        eve = {
          isNormalUser = true;
          uid = eveUid;
          group = "eve";
        };
      };
      users.groups = {
        frontdoor = { };
        alice.gid = aliceUid;
        mallory.gid = malloryUid;
        eve.gid = eveUid;
      };

      services.openssh = {
        enable = true;
        listenAddresses = [
          {
            addr = "127.0.0.1";
            port = 22;
          }
        ];
      };

      services.spire.server = {
        enable = true;
        settings = {
          server = {
            trust_domain = trustDomain;
            bind_address = "127.0.0.1";
            bind_port = 8081;
          };
          plugins = {
            DataStore.sql.plugin_data = {
              database_type = "sqlite3";
              connection_string = "$STATE_DIRECTORY/datastore.sqlite3";
            };
            KeyManager.disk.plugin_data = {
              keys_path = "$STATE_DIRECTORY/keys.json";
            };
            NodeAttestor.x509pop.plugin_data = {
              ca_bundle_path = "${testCerts}/ca.crt";
            };
          };
        };
      };

      services.spire.agent = {
        enable = true;
        settings = {
          agent = {
            trust_domain = trustDomain;
            server_address = "127.0.0.1";
            server_port = 8081;
            socket_path = spireAgentSocket;
            trust_bundle_path = agentBundle;
          };
          plugins = {
            KeyManager.memory.plugin_data = { };
            NodeAttestor.x509pop.plugin_data = {
              private_key_path = "${testCerts}/agent.key";
              certificate_path = "${testCerts}/agent.crt";
            };
            WorkloadAttestor.unix.plugin_data = { };
          };
        };
      };

      systemd.services.spire-agent = {
        after = [ "spire-server.service" ];
        wants = [ "spire-server.service" ];
        serviceConfig.ExecStartPre = pkgs.lib.mkBefore [ "+${bootstrap}" ];
      };

      systemd.services.frontdoor = {
        description = "SPIFFE mTLS ghostunnel server to local sshd";
        wantedBy = [ "multi-user.target" ];
        after = [
          "spire-agent.service"
          "sshd.service"
        ];
        requires = [
          "spire-agent.service"
          "sshd.service"
        ];
        serviceConfig = {
          User = "frontdoor";
          Group = "frontdoor";
          SupplementaryGroups = [ "spire-agent" ];
          ExecStart = ''
            ${ghostunnel}/bin/ghostunnel server \
              --use-workload-api-addr unix://${spireAgentSocket} \
              --listen ${frontdoorListen} \
              --target 127.0.0.1:22 \
              --allow-uri ${aliceID}
          '';
          Restart = "on-failure";
          RestartSec = 1;
        };
      };

      environment.systemPackages = [
        spiffec
        pkgs.openssh
        pkgs.util-linux
      ];
    };

  testScript = ''
    start_all()

    machine.wait_for_unit("spire-server.service", timeout=120)
    machine.wait_until_succeeds(
        "spire-server healthcheck -socketPath ${spireServerSocket}", timeout=120
    )
    machine.wait_for_unit("spire-agent.service", timeout=180)
    machine.wait_until_succeeds(
        "spire-agent healthcheck -socketPath ${spireAgentSocket}", timeout=180
    )
    machine.wait_for_unit("sshd.service", timeout=120)
    machine.wait_for_unit("frontdoor.service", timeout=180)
    machine.wait_for_open_port(2222, timeout=180)

    setpriv = "${pkgs.util-linux}/bin/setpriv"
    spiffec_bin = "${spiffec}/bin/spiffec"
    addr = "${frontdoorListen}"
    server_id = "${serverID}"

    def as_uid(uid, cmd):
        return f"{setpriv} --reuid={uid} --regid={uid} --init-groups -- {cmd}"

    with subtest("usage errors exit 2"):
        rc, _ = machine.execute(f"{spiffec_bin} 2>/dev/null")
        assert rc == 2, f"expected 2, got {rc}"
        rc, _ = machine.execute(
            f"{spiffec_bin} -addr nonsense -server-id {server_id} 2>/dev/null"
        )
        assert rc == 2, f"expected 2, got {rc}"
        rc, _ = machine.execute(
            f"{spiffec_bin} -addr {addr} -server-id not-a-spiffe-id 2>/dev/null"
        )
        assert rc == 2, f"expected 2, got {rc}"

    with subtest("alice tunnels over spiffec and drains response after stdin EOF"):
        # stdin reaches EOF immediately after printf; spiffec half-closes the
        # TLS connection and must still drain sshd's greeting banner on stdout.
        banner = machine.wait_until_succeeds(
            as_uid(
                ${toString aliceUid},
                f"sh -c 'printf \"SSH-2.0-spiffec-test\\r\\n\" | "
                f"{spiffec_bin} -addr {addr} -server-id {server_id}'",
            ),
            timeout=60,
        )
        assert banner.startswith("SSH-2.0-"), f"unexpected banner: {banner!r}"

    with subtest("alice runs OpenSSH over spiffec ProxyCommand"):
        machine.succeed(
            "install -m 0400 -o alice -g alice ${testCerts}/alice_id_ed25519 /tmp/alice_key"
        )
        out = machine.succeed(
            as_uid(
                ${toString aliceUid},
                "ssh -F /dev/null "
                "-o StrictHostKeyChecking=no "
                "-o UserKnownHostsFile=/dev/null "
                "-o BatchMode=yes "
                "-i /tmp/alice_key "
                f"-o 'ProxyCommand={spiffec_bin} -addr {addr} -server-id {server_id}' "
                "alice@localhost printf spiffec-ssh-ok",
            ),
            timeout=60,
        )
        assert out == "spiffec-ssh-ok", f"unexpected ssh stdout: {out!r}"

    with subtest("mismatched server SPIFFE ID is rejected"):
        rc, _ = machine.execute(
            as_uid(
                ${toString aliceUid},
                f"{spiffec_bin} -addr {addr} "
                "-server-id spiffe://example.org/server/wrong </dev/null 2>/dev/null",
            ),
            timeout=60,
        )
        assert rc == 1, f"expected 1, got {rc}"

    with subtest("non-allowlisted SPIFFE identity is rejected"):
        out = machine.succeed(
            as_uid(
                ${toString malloryUid},
                f"sh -c 'printf \"SSH-2.0-mallory\\r\\n\" | "
                f"{spiffec_bin} -addr {addr} -server-id {server_id} 2>/dev/null || true'",
            ),
            timeout=60,
        )
        assert out == "", f"mallory should receive no payload bytes, got {out!r}"
        machine.succeed("systemctl is-active frontdoor.service")

    with subtest("unregistered user without an SVID fails with exit 1"):
        rc, err = machine.execute(
            as_uid(
                ${toString eveUid},
                f"{spiffec_bin} -addr {addr} -server-id {server_id} </dev/null 2>&1",
            ),
            timeout=60,
        )
        assert rc == 1, f"expected 1, got {rc}: {err}"
        assert "X509-SVID" in err, f"unexpected error output: {err!r}"
  '';

  meta.timeout = 600;
}
