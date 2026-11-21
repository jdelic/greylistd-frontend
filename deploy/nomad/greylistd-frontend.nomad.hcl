# Example Nomad job for greylistd-frontend.
#
# Assumptions (adapt to your cluster):
#   * The job runs on the mail server itself, because it needs greylistd's
#     UNIX socket. The constraint below pins it to nodes whose client config
#     sets `meta { greylistd = "true" }`.
#   * The Docker driver may bind-mount host paths
#     (plugin "docker" { config { volumes { enabled = true } } }).
#   * greylistd runs as opensmtpd:opensmtpd (uid/gid 108 on Debian 13 here;
#     check with `id opensmtpd`) and its socket is mode 0660, so the task runs
#     with that uid/gid.
#   * Secrets come from Vault (workload identity). Replace the template with
#     plain `env` values if you do not use Vault.
#   * A reverse proxy (haproxy/fabio/traefik/...) terminates TLS for
#     https://greylist.example.com and forwards to the registered service.

job "greylistd-frontend" {
  datacenters = ["dc1"]
  type        = "service"

  constraint {
    attribute = "${meta.greylistd}"
    value     = "true"
  }

  group "web" {
    count = 1

    # Give Consul/the proxy time to stop routing traffic before shutdown.
    shutdown_delay = "5s"

    network {
      port "http" {
        to = 8080
      }
    }

    service {
      name     = "greylistd-frontend"
      port     = "http"
      provider = "consul"
      tags     = ["http"]

      check {
        type     = "http"
        path     = "/healthz"
        interval = "15s"
        timeout  = "3s"
      }
    }

    restart {
      attempts = 5
      interval = "5m"
      delay    = "10s"
      mode     = "delay"
    }

    task "greylistd-frontend" {
      driver = "docker"

      # uid:gid owning /var/run/greylistd/socket (opensmtpd:opensmtpd).
      user = "108:108"

      config {
        # Built and pushed by .github/workflows/release.yml; use your
        # REGISTRY/IMAGE_NAME settings here.
        image = "ghcr.io/jdelic/greylistd-frontend:0.1.0"
        ports = ["http"]

        # greylistd's socket directory. Mounting the directory (not the
        # socket file) keeps working when greylistd re-creates the socket.
        volumes = [
          "/var/run/greylistd:/var/run/greylistd",
        ]

        readonly_rootfs = true
        cap_drop        = ["ALL"]
        security_opt    = ["no-new-privileges"]
      }

      env {
        GREYLISTD_FRONTEND_LISTEN   = ":8080"
        GREYLISTD_FRONTEND_SOCKET   = "/var/run/greylistd/socket"
        GREYLISTD_FRONTEND_BASE_URL = "https://greylist.example.com"
        # Must match the time zone of the host greylistd runs on.
        GREYLISTD_FRONTEND_TIMEZONE = "UTC"
        # The proxy in front sets X-Forwarded-For.
        GREYLISTD_FRONTEND_TRUST_PROXY = "true"

        GREYLISTD_FRONTEND_OIDC_ISSUER         = "https://auth.example.com/o2"
        GREYLISTD_FRONTEND_OIDC_CLIENT_ID      = "greylistd-frontend"
        GREYLISTD_FRONTEND_OIDC_PROVIDER_NAME  = "maurus.networks"
        GREYLISTD_FRONTEND_OIDC_ALLOWED_EMAILS = "postmaster@example.com"
        # Alternatively/additionally allow members of a group:
        # GREYLISTD_FRONTEND_OIDC_ALLOWED_GROUPS = "mailadmins"
        # GREYLISTD_FRONTEND_OIDC_SCOPES         = "openid,email,profile,groups"
      }

      # Secrets: OIDC client secret, cookie signing key and the optional
      # break-glass password login. Remove the AUTH_* lines to disable the
      # password login.
      vault {}

      template {
        destination = "${NOMAD_SECRETS_DIR}/env"
        env         = true
        change_mode = "restart"
        data        = <<-EOT
          {{ with secret "secret/data/greylistd-frontend" -}}
          GREYLISTD_FRONTEND_OIDC_CLIENT_SECRET={{ .Data.data.oidc_client_secret }}
          GREYLISTD_FRONTEND_SESSION_SECRET={{ .Data.data.session_secret }}
          GREYLISTD_FRONTEND_AUTH_USERNAME={{ .Data.data.auth_username }}
          GREYLISTD_FRONTEND_AUTH_PASSWORD={{ .Data.data.auth_password }}
          {{- end }}
        EOT
      }

      resources {
        cpu    = 50
        memory = 64
      }

      logs {
        max_files     = 5
        max_file_size = 10
      }
    }
  }
}
