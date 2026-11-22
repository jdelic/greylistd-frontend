greylistd-frontend
==================

A small, mobile-first web interface for `greylistd(8)`_. It talks to
greylistd directly over its UNIX socket and lets you

* list all white-, grey- and blacklisted entries, filter them and sort them by
  domain/IP, list, last-seen time or hit count,
* whitelist, blacklist or delete entries one by one or in bulk,
* add new domains or IP addresses,
* see greylistd's statistics.

Login is via OpenID Connect and/or an optional static username and password.
The server is a single static Go binary with all assets embedded, so it runs
in a ``FROM scratch`` container.

It is meant as the companion of `opensmtpd-filter-greylistd`_: when you are
waiting for an e-mail from a sender that is stuck in greylisting, open the
page on your phone and whitelist the domain instead of running
``greylist add`` over SSH.


How whitelisting interacts with filter-greylistd
------------------------------------------------

``filter-greylistd`` first checks the sending host's reverse DNS name: if its
registrable domain (eTLD+1, e.g. ``example.com``) or the name one level below
(eTLD+2, e.g. ``mail.example.com``) is whitelisted, the mail passes. Otherwise
it asks greylistd about the SPF-validated sender domain, or the IP address if
SPF fails. So whitelisting ``example.com`` lets mail from hosts like
``mx1.example.com`` through.

greylistd only reports entries for which it retained the original text
(``savetriplets = true``, the default). Entries are lower-cased by greylistd;
the UI does the same.


Configuration
-------------

Every option can be given as a flag or as an environment variable named
``GREYLISTD_FRONTEND_<FLAG NAME>`` (upper case, ``-`` becomes ``_``). Flags
win over the environment. Run ``greylistd-frontend --help`` for the full list.

=========================  =============================  ==================================================
Flag                       Default                        Meaning
=========================  =============================  ==================================================
``--listen``               ``:8080``                      HTTP listen address
``--socket``               ``/var/run/greylistd/socket``  greylistd's socket
``--socket-timeout``       ``10s``                        timeout of one greylistd command
``--timezone``             ``$TZ``, then ``UTC``          time zone of the **greylistd host**; greylistd
                                                          prints local timestamps without a zone
``--base-url``                                            public URL, e.g. ``https://greylist.example.com``;
                                                          required for OIDC, ``https://`` makes cookies
                                                          ``Secure``
``--trust-proxy``          ``false``                      use ``X-Forwarded-For`` as the client address
``--log-level``            ``info``                       ``debug``, ``info``, ``warn``, ``error``
``--session-secret``       random                         >= 32 bytes (raw, hex or base64) to sign
                                                          cookies; set it, or every restart logs you out
``--session-ttl``          ``12h``                        session lifetime
``--oidc-issuer``                                         issuer URL; enables OIDC login
``--oidc-client-id``                                      client ID
``--oidc-client-secret``                                  client secret
``--oidc-scopes``          ``openid,email,profile``       requested scopes
``--oidc-allowed-emails``                                 comma-separated e-mail allow list
``--oidc-allowed-groups``                                 comma-separated group allow list
``--oidc-groups-claim``    ``groups``                     claim holding the groups
``--oidc-allow-any-user``  ``false``                      allow everybody the IdP authenticates
``--oidc-provider-name``   ``OpenID Connect``             label of the login button
``--auth-username``                                       username of the optional password login
``--auth-password``                                       its password (use the environment variable)
``--allow-anonymous``      ``false``                      without any login method: everybody gets full
                                                          access (only behind other access control)
=========================  =============================  ==================================================

If OIDC is the only login method, the login page redirects straight to the
identity provider (except right after logging out, so that the provider's
session does not log you back in immediately). With a password login as well,
the login page offers both.

Without any login method the UI only shows an error saying so, unless
``--allow-anonymous`` is set: then everybody who can reach the server has full
access without logging in. Use that only when something else, such as a
reverse proxy with its own authentication or a private network, controls
access. ``--allow-anonymous`` cannot be combined with a login method.

OIDC refuses to start without an allow list (e-mails and/or groups) unless
``--oidc-allow-any-user`` is set explicitly, because whoever can log in can
change your mail server's greylisting.

The OIDC redirect URI to register with your provider is
``<base-url>/auth/oidc/callback``. The login uses the authorization code flow
with PKCE (S256) and a nonce; a confidential client is expected.


Logging
-------

Both logs are JSON lines:

* **stdout** – access log, one line per request (client, method, path,
  status, bytes, duration, user agent, referer).
* **stderr** – application log (start-up, logins, every change to greylistd
  with the acting user, errors).


Endpoints
---------

``/healthz`` is unauthenticated and returns ``200 ok`` when greylistd answers
on its socket, ``503`` otherwise. Everything else besides ``/login``, the OIDC
callback and ``/static/`` requires a session.


Building
--------

::

    go build -o greylistd-frontend ./cmd/greylistd-frontend
    go test ./...

Container image::

    docker build --build-arg VERSION=0.1.0 -t greylistd-frontend:0.1.0 .

The image contains only the binary and a CA bundle. greylistd's socket is
mode ``0660`` and owned by the user greylistd runs as (``opensmtpd`` when used
with OpenSMTPD), so run the container with that uid/gid and mount the socket's
directory::

    docker run --user "$(id -u opensmtpd):$(id -g opensmtpd)" \
        -v /var/run/greylistd:/var/run/greylistd \
        -p 8080:8080 \
        -e GREYLISTD_FRONTEND_AUTH_USERNAME=admin \
        -e GREYLISTD_FRONTEND_AUTH_PASSWORD=... \
        -e GREYLISTD_FRONTEND_SESSION_SECRET="$(openssl rand -hex 32)" \
        greylistd-frontend:0.1.0

If your OIDC provider uses a private CA, mount a bundle containing it over
``/etc/ssl/certs/ca-certificates.crt`` (or point ``SSL_CERT_FILE`` at it).


Continuous integration and releases
-----------------------------------

GitHub Actions (``.github/workflows``):

``ci.yml``
    Runs on every push to ``master`` and on pull requests: ``go mod tidy``
    check, `golangci-lint`_ (config in ``.golangci.yml``, includes gofmt),
    ``go vet``, ``go test -race``, a build, and a Docker image build with a
    smoke test of the container.

``release.yml``
    Runs on tags ``v*`` (e.g. ``git tag -a v0.1.0 -m "..." && git push --tags``).
    It runs the CI checks first, then

    * builds a multi-arch image (``linux/amd64``, ``linux/arm64``) and pushes
      it tagged ``1.2.3``, ``1.2``, ``1`` (not for ``0.x``) and ``latest`` (not
      for pre-releases such as ``v1.3.0-rc1``);
    * builds static ``linux/amd64`` and ``linux/arm64`` binaries and publishes
      them as a GitHub release for the tag (a pre-release for tags such as
      ``v1.3.0-rc1``): ``greylistd-frontend_1.2.3_linux_amd64.tar.gz`` etc.
      contain the binary, the READMEs and the systemd example from
      ``deploy/systemd``; ``SHA256SUMS`` lists their checksums. Re-running the
      workflow replaces the files of an existing release.

The release job runs in the GitHub environment ``release`` and is configured
under *Settings → Secrets and variables → Actions*, either repository-wide or
on that environment:

=========================  ========  ===========================================
Name                       Kind      Meaning (default)
=========================  ========  ===========================================
``REGISTRY``               variable  registry host (``ghcr.io``)
``IMAGE_NAME``             variable  image path without host (``<owner>/<repo>``)
``PLATFORMS``              variable  build platforms (``linux/amd64,linux/arm64``)
``REGISTRY_USERNAME``      secret    registry user (the workflow actor)
``REGISTRY_PASSWORD``      secret    registry password or token (``GITHUB_TOKEN``,
                                     which only works for ``ghcr.io``)
=========================  ========  ===========================================

With nothing configured, images go to ``ghcr.io/<owner>/<repo>``. The
GitHub release needs no configuration; it uses ``GITHUB_TOKEN``.

Dependabot (``.github/dependabot.yml``) opens weekly, grouped pull requests
for Go module and GitHub Actions updates.

systemd
-------

greylistd-frontend has to run on the mail server, next to greylistd's socket.
Without a container platform there, run the release binary as a systemd
service. ``deploy/systemd`` has an example unit and environment file::

    tar xzf greylistd-frontend_1.2.3_linux_amd64.tar.gz
    cd greylistd-frontend_1.2.3_linux_amd64
    install -m 0755 greylistd-frontend /usr/local/bin/
    install -m 0644 systemd/greylistd-frontend.service /etc/systemd/system/
    install -d -m 0750 /etc/greylistd-frontend
    install -m 0600 systemd/greylistd-frontend.env /etc/greylistd-frontend/env
    $EDITOR /etc/greylistd-frontend/env
    systemctl daemon-reload && systemctl enable --now greylistd-frontend

The unit runs the service as a ``DynamicUser`` whose only extra right is
membership in the socket's group, with systemd's sandboxing options switched
on. Debian's ``greylistd.socket`` uses group ``greylist``; setups with
opensmtpd-filter-greylistd usually change it to ``opensmtpd``, which is what
the example assumes. Adjust ``SupplementaryGroups=`` to
``stat -c %G /var/run/greylistd/socket``.

Set ``GREYLISTD_FRONTEND_TIMEZONE`` in the environment file: systemd services
get no ``$TZ``, so the frontend would otherwise read greylistd's timestamps as
UTC. The example listens on ``127.0.0.1:8080`` for a reverse proxy on the same
host.

Nomad
-----

``deploy/nomad/greylistd-frontend.nomad.hcl`` is an example job: Docker
driver, socket directory bind mount, task uid/gid of ``opensmtpd``, a Consul
service with ``/healthz`` check, and secrets rendered from Vault.


Security notes
--------------

* Sessions are stateless HMAC-signed cookies (``HttpOnly``,
  ``SameSite=Lax``, ``Secure`` with an ``https`` base URL). Logging out clears
  the cookie; rotating ``--session-secret`` invalidates all sessions.
* Every state-changing form carries a per-session CSRF token; the password
  form uses a double-submit token.
* Failed password logins are rate-limited per client address (5 per 5
  minutes).
* Input sent to greylistd is validated (no option injection such as
  ``--white``, no control characters).
* Responses carry a strict Content-Security-Policy; the UI loads no
  third-party resources.


Credits
-------

The look follows the admin UI of artifactsd. It uses the `VT323`_ font by
Peter Hull, licensed under the SIL Open Font License 1.1.

.. _greylistd(8): https://packages.debian.org/trixie/greylistd
.. _opensmtpd-filter-greylistd: https://github.com/jdelic/opensmtpd-filter-greylistd
.. _golangci-lint: https://golangci-lint.run/
.. _VT323: https://fonts.google.com/specimen/VT323
