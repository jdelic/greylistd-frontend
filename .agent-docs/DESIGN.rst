===================================
greylistd-frontend: design document
===================================

:Status: implemented (master, commit 063c0cb plus the login-mode change
         described in `Changes from the conversation`_)
:Spec: ``.agent-docs/SPEC.rst``
:Last updated: 2026-11-21

.. contents::
   :local:
   :depth: 2


Purpose
=======

A small web UI to whitelist, blacklist or delete greylistd entries from a
phone, instead of logging in over SSH and running ``greylist add``. It runs
next to OpenSMTPD and ``opensmtpd-filter-greylistd`` on the mail server and
talks to greylistd directly over its UNIX socket.

Requirements from ``SPEC.rst``, and where they are met:

===================================================  =============================================
Requirement                                          Implementation
===================================================  =============================================
List grey/white/black entries, sortable, filterable  ``GET /``, `Entries page`_
Whitelist/blacklist via buttons and bulk selection   ``POST /entries/action``, `Actions`_
Simple form to add domains                           ``POST /entries/add``
OpenID Connect login                                 ``internal/auth/oidc.go``, `Authentication`_
Optional username/password from the environment      ``--auth-username``/``--auth-password``
Config via flags or 12factor environment variables   ``internal/config``, `Configuration`_
Access log on stdout, JSON app log on stderr         ``internal/logging``, `Logging`_
``FROM scratch`` container                           ``Dockerfile``, `Packaging`_
Example Nomad job                                    ``deploy/nomad/greylistd-frontend.nomad.hcl``
Example systemd unit, release binaries, .deb         ``deploy/systemd``, ``Makefile``, `systemd`_
Mobile-first, browser-friendly, artifactsd look      `User interface`_
===================================================  =============================================


Architecture
============

One static Go binary (Go version per ``go.mod``, ``CGO_ENABLED=0``). Templates, CSS, JS, the
favicon and the font are embedded with ``embed``, and so is the time zone
database (``time/tzdata``), because a scratch image has no
``/usr/share/zoneinfo``.

::

    cmd/greylistd-frontend/main.go   wiring, startup checks, graceful shutdown
    internal/config                  flags + GREYLISTD_FRONTEND_* environment
    internal/greylistd               socket client and input validation
    internal/auth                    signed cookies, sessions, password, OIDC
    internal/logging                 slog JSON handlers, access log middleware
    internal/web                     routes, handlers, templates, static files

Request path: ``http.Server`` (with ``MaxBytesHandler`` 1 MiB and
read/write/idle timeouts) → access-log middleware → ``web.Server``
(security headers) → ``ServeMux`` (Go 1.22 method patterns) →
``requireSession`` → handler.

External dependencies are deliberately few: ``github.com/coreos/go-oidc/v3``
and ``golang.org/x/oauth2`` (plus go-jose, pulled in by go-oidc). Everything
else is the standard library. The UI loads no third-party resources.


greylistd client
================

``internal/greylistd`` implements greylistd's socket protocol, which I
worked out from greylistd's source and by trying it on the VM:

* One command per connection: connect, write the command line,
  ``CloseWrite``, read the reply until EOF. Every call has a deadline
  (``--socket-timeout``, default 10s).
* Commands used: ``list [--white|--grey|--black]``, ``add --<list> <data>``,
  ``delete <data>``, ``stats``, and ``status ping.invalid`` as a cheap
  liveness probe for ``/healthz``.
* Error replies start with ``error:``. They become ``*greylistd.Error``, and
  ``delete``'s "Not found" becomes ``ErrNotFound``.
* ``list`` reports "No such file or directory" when greylistd has never
  written its triplets file. That is treated as an empty list, not an error.
* ``add --<list>`` moves an existing entry to that list. The UI relies on
  this for "move to white/black/grey".
* ``list`` output is parsed into ``Entry{List, Data, LastSeen, Count}``. The
  data may contain spaces (triplets), so the parser takes the timestamp and
  count from the right.

Input validation (``NormalizeData``):

* The input is lowercased, as greylistd does, and whitespace is collapsed.
  The length is capped at 512 characters.
* A leading ``-`` is rejected, so data cannot become an option such as
  ``--white``.
* Only ``[a-z0-9._:@+=/[]-]`` is allowed, plus spaces and ``<>`` after the
  first word, for triplets. Control characters are rejected.
* The value is validated twice: in the handler, and again in the client
  before anything is written to the socket.


User interface
==============

Pages
-----

========================  ===================================================
Route                     Purpose
========================  ===================================================
``GET /login``            login page (or a redirect, see `Login modes`_)
``POST /login``           password login
``GET /auth/oidc/*``      OIDC start and callback
``POST /logout``          logout
``GET /``                 entries page
``POST /entries/add``     add one or more entries to a list
``POST /entries/action``  row or bulk action
``GET /stats``            greylistd's ``stats`` output
``GET /healthz``          unauthenticated, 200 or 503 depending on greylistd
``GET /static/``          embedded assets, cache-busted with ``?v=<version>``
========================  ===================================================

Entries page
------------

* **Add panel:** a free-text field that splits on spaces, commas,
  semicolons and newlines (at most 500 items), a list selector and an Add
  button. A hint explains the eTLD+1/eTLD+2 matching of filter-greylistd.
* **Filter:** the query parameter ``q`` filters on the server, by substring
  of the data. With JS the rows also filter live while typing. If the filter
  is broadened beyond what the server returned, the page reloads with the
  new ``q``.
* **Tabs:** All, White, Grey and Black, each with a count (``list``
  parameter).
* **Sorting:** by ``sort`` = ``data|list|seen|count`` and ``dir``. The
  default is last seen, newest first, which is what you want when waiting
  for a mail. The data sort puts IP addresses first, in numeric order, then
  domains by their reversed labels, so ``mail.example.com`` sorts next to
  ``example.com``.
* **Layout:** on phones each entry is a card with a checkbox, data, list
  badge, last-seen time (absolute plus "7h ago"), hits and per-row buttons.
  From 760px wide it becomes a classic table with sortable column headers.
  On phones the sort links live in a separate sort bar.
* **Bulk bar:** sticky at the bottom on mobile. It shows "n selected" and
  buttons for Whitelist, Blacklist and Delete, disabled while nothing is
  selected.

Actions
-------

All of them are one form, ``#entries-form``, posting to ``/entries/action``:

* Row buttons send ``row=<action>:<data>``.
* Bulk buttons send ``bulk=<action>`` plus the checked ``sel`` values.
* Actions are ``white``, ``grey``, ``black`` (all via ``add``) and
  ``delete``.

Every change is logged with the acting user. The result is shown on the
next page as a flash message, via the signed cookie ``glf_flash``
(Post/Redirect/Get). Successes and failures are reported separately.

The redirect target comes from the hidden ``back`` field. ``backURL`` only
accepts the local path ``/`` and rebuilds the query from the known
parameters, so it cannot be used as an open redirect.

Without JavaScript
------------------

Everything works without JS: forms post, tabs and sort are links, and the
filter is a GET form. ``app.js`` adds:

* Alt+letter shortcuts;
* confirmation dialogs for delete and bulk actions;
* live filtering;
* select-all with a selection counter;
* enabling and disabling the bulk buttons;
* keeping the ``back`` field and the tab/sort links in sync with the live
  filter.


Taken over from artifactsd
==========================

The spec asked for artifactsd's design language, used as a reference only.
No artifactsd code is imported; the CSS and JS were written fresh for this
project, modelled on ``~/artifactsd/internal/admin``.

What was taken over:

* **The look:** a 90s DOS/Norton-Commander TUI.

  * A blue screen with a cyan top menu bar and a cyan status/footer bar.
  * Ridge-bordered panels with the title set into the top border
    (``data-label``).
  * Bevelled buttons, yellow highlights for data, the ``nc-`` class prefix,
    and CSS variables of the form ``--nc-*``.

* **The palette:** the 16-colour CGA/VGA values, i.e. ``--nc-blue``
  ``#0000a8``, ``--nc-cyan`` ``#00a8a8``, ``--nc-cyan-bright``,
  ``--nc-yellow``, ``--nc-red``, ``--nc-green``, ``--nc-grey`` and so on.
* **The font:** VT323 (SIL OFL 1.1). The ``vt323.woff2`` file was copied
  from artifactsd and is embedded and self-hosted, with no Google Fonts.
  Credited in the README.
* **The favicon:** an SVG in the same style.
* **Keyboard shortcuts:** Alt+letter shortcuts through ``data-shortcut``
  attributes and an underlined accelerator letter in the label (``<u>E</u>ntries``).
  In artifactsd this is ``shortcuts.js``; here it is part of ``app.js``.
* **Footer:** a function-key style hint strip (``Alt+A Add`` …), the
  version, and a status cell on the right (``READY``, ``ERROR``,
  ``n ITEMS``).
* **Server rendering:** HTML rendered on the server with ``html/template``,
  a layout with ``head``/``foot`` blocks, embedded static files, and
  version-tagged asset URLs.
* **CSP:** a strict Content-Security-Policy with no inline scripts or
  styles.

What was deliberately *not* taken over:

* **htmx:** artifactsd ships htmx; this UI has few enough interactions that
  plain forms plus about 130 lines of vanilla JS suffice, and it works
  without JS.
* **Layout:** artifactsd's admin is desktop-first. This UI is mobile-first,
  with cards on phones, a table on desktop, larger touch targets and a
  sticky bulk bar.
* **Auth, storage and API:** none of artifactsd's auth, storage or API code
  is used.


Authentication
==============

Sessions
--------

* **Stateless sessions:** there is no server-side store. A session is a
  cookie (``glf_session``) holding JSON with subject, display name, method,
  CSRF token and expiry, signed with HMAC-SHA256.
* **Purpose binding:** the purpose (``session``, ``oidc-flow``, ``flash``)
  is part of the MAC input, so a value signed for one purpose is never
  accepted as another.
* **Cookie attributes:** ``HttpOnly`` and ``SameSite=Lax``. ``Secure`` is
  set exactly when ``--base-url`` is ``https://``, so the plain-HTTP setup
  on localhost still works.
* **Key:** ``--session-secret`` (≥ 32 bytes; raw, hex or base64). Without
  it a random key is generated and every restart logs everybody out; a
  warning is logged.
* **Lifetime:** ``--session-ttl``, default 12h.
* **CSRF protection:**

  * Every POST behind a session must carry the session's CSRF token.
  * The password form has no session yet, so it uses a double-submit token
    (cookie ``glf_login_csrf``).

* **Methods must still be enabled:** a session is accepted only if the
  method that created it (``oidc``, ``password``, ``anonymous``) is still
  enabled. Turning a method off, e.g. ``--allow-anonymous``, therefore
  invalidates its sessions immediately.

Password login
--------------

* **Configuration:** ``--auth-username`` and ``--auth-password``, best
  given as environment variables. They must be set together.
* **Comparison:** constant time, on SHA-256 hashes of username and
  password.
* **Rate limiting:** 5 failures per 5 minutes per client address, in
  memory. Each failure also adds a 500ms delay. The client address comes
  from ``X-Forwarded-For`` only with ``--trust-proxy``.

OpenID Connect
--------------

* **Flow:** the authorization code flow with PKCE (S256), ``state`` and a
  ``nonce``, using a confidential client.
* **Flow cookie:** the flow state (state, nonce, PKCE verifier, expiry) is
  kept in a signed, short-lived (10 min) cookie ``glf_oidc``.
* **Discovery:** happens lazily on first use, so the server starts even
  when the IdP is down. The login page then shows "identity provider
  unreachable".
* **Claims:** claims from the ID token are merged with userinfo.
* **Authorization:**

  * The user's e-mail must be in ``--oidc-allowed-emails``, or one of their
    groups (``--oidc-groups-claim``, default ``groups``) in
    ``--oidc-allowed-groups``.
  * Mail-based allowing is refused when ``email_verified`` is explicitly
    false.
  * Startup fails without an allow list unless ``--oidc-allow-any-user`` is
    given, because whoever logs in controls the mail server's greylisting.

* **Redirect URI:** ``<base-url>/auth/oidc/callback``, so ``--base-url`` is
  required with OIDC.

Login modes
-----------

========================  ======================================================
Configured                Behaviour of ``/login`` and protected pages
========================  ======================================================
OIDC only                 ``/login`` redirects straight to the IdP. Exception:
                          after logout (``/login?logged-out``) the page shows
                          "You have been logged out." and a sign-in button, as
                          the IdP session would otherwise log the user straight
                          back in. Error pages (denied, IdP down, expired flow)
                          are rendered directly and never auto-redirect, so
                          there is no redirect loop.
Password only             login form
OIDC + password           both, separated by "or"
Nothing                   server starts (logs an error); ``/login`` shows "No
                          login method is configured…" with HTTP 503; every
                          protected page redirects there; ``/healthz`` still
                          works
Nothing +                 no login: the first GET creates an ``anonymous``
``--allow-anonymous``     session (needed to carry the CSRF token), the Logout
                          button is hidden, ``/login`` redirects to ``/``. A
                          warning is logged at startup
Login + allow-anonymous   configuration error, refuses to start
========================  ======================================================


Configuration
=============

* **Sources:** every flag ``--some-name`` can also be set through
  ``GREYLISTD_FRONTEND_SOME_NAME``. Precedence is flag, then environment,
  then default. This is implemented on top of ``flag`` by filling unset
  flags from the environment after parsing.
* **Validation:** problems are collected and reported together with
  ``errors.Join``. The process exits with code 2 and a JSON error line.
* **Time zone:** ``--timezone`` defaults to ``$TZ``, then UTC. It must be
  the zone of the greylistd host, because greylistd prints local
  timestamps without a zone.
* **Reference:** the README has the full flag table.


Logging
=======

* **Format:** both logs use ``log/slog`` with JSON handlers.
* **stdout, access log:** one line per request with client, method, path,
  protocol, status, bytes, ``duration_ms``, user agent and referer. The
  level field is dropped, since it carries no information there.
* **stderr, application log:** startup (with the enabled login methods),
  config warnings, logins and logouts, every greylistd change with the
  acting user, CSRF mismatches, rate limiting, failed health checks and
  errors. The level is set with ``--log-level``.


Packaging
=========

Docker
------

* **Build:** a multi-stage build. The build stage is
  ``golang:1.26-trixie`` with ``--platform=$BUILDPLATFORM``, cross-compiling
  via ``TARGETOS``/``TARGETARCH``, so arm64 images build without emulating
  the compiler. ``-trimpath`` is used, and ``VERSION`` is set via ldflags.
* **Runtime:** ``FROM scratch`` containing only the binary and
  ``/etc/ssl/certs/ca-certificates.crt``, which the OIDC client needs.
  Runs as ``USER 65534:65534`` and exposes 8080.
* **Socket access:** the greylistd socket is mode 0660 and owned by
  ``opensmtpd``, so deployments must run the container with that uid/gid
  (108:108 on the VM) and bind-mount ``/var/run/greylistd``.

systemd
-------

The service must run on the greylistd host, which may have no container
platform, so the release also ships static binaries and
``deploy/systemd/greylistd-frontend.service`` plus an environment file:

* **User:** ``DynamicUser=yes`` with ``SupplementaryGroups=`` set to the
  socket's group (``opensmtpd`` in the example; Debian's default
  ``greylistd.socket`` uses ``greylist``). Socket access through the group
  avoids running as the mail daemon's user.
* **Ordering:** ``After=``/``Wants=greylistd.socket``, because greylistd is
  socket-activated on Debian.
* **Secrets:** ``EnvironmentFile=/etc/greylistd-frontend/env`` (mode 0600,
  read by systemd as root).
* **Sandboxing:** ``ProtectSystem=strict``, no capabilities, restricted
  address families (``AF_UNIX``/``AF_INET``/``AF_INET6``), and a
  ``@system-service`` syscall filter. ``systemd-analyze security`` rates it
  1.3. It was tested against the VM's greylistd (``/healthz`` 200).
* **Time zone:** systemd sets no ``$TZ``, so the environment file sets
  ``GREYLISTD_FRONTEND_TIMEZONE`` explicitly.

Nomad
-----

``deploy/nomad/greylistd-frontend.nomad.hcl`` (passes
``nomad job validate``):

* **Placement:** the Docker driver, constrained to nodes with
  ``meta.greylistd = "true"``.
* **Container settings:**

  * The socket directory is bind-mounted, and the task runs as
    ``user = "108:108"``.
  * ``readonly_rootfs`` is on, all capabilities are dropped, and
    ``shutdown_delay`` is 5s.

* **Service:** a Consul service with an HTTP check on ``/healthz``.
* **Secrets:** rendered from Vault (workload identity) into environment
  variables.
* **Image:** ``ghcr.io/jdelic/greylistd-frontend:0.1.0`` is a placeholder.

CI/CD (GitHub Actions)
----------------------

* ``ci.yml`` runs on pushes to ``master`` and on PRs, and is also
  callable. Its jobs:

  * a ``go mod tidy`` diff check;
  * golangci-lint v2.15 (config ``.golangci.yml``: standard linters plus
    bodyclose, errorlint, gosec, misspell, nilerr, noctx and unconvert,
    with the gofmt formatter);
  * ``go vet``, ``go test -race`` and a build;
  * a Docker build with a smoke test: ``--version``, the login page
    renders, and ``/healthz`` is 503 because there is no greylistd.

* ``release.yml`` runs on ``v*`` tags:

  * It reuses ``ci.yml`` as a gate, then builds amd64 and arm64 images and
    pushes them.
  * Images are tagged with docker/metadata-action semver tags. ``{{major}}``
    is skipped for 0.x, and ``latest`` is not set for pre-releases.
  * The job runs in the GitHub environment ``release``.
  * The registry comes from ``vars.REGISTRY``, ``vars.IMAGE_NAME``,
    ``vars.PLATFORMS``, ``secrets.REGISTRY_USERNAME`` and
    ``secrets.REGISTRY_PASSWORD``. Unset values fall back to ghcr.io and
    ``GITHUB_TOKEN``.
  * In parallel, a matrix job runs ``make dist deb`` for ``linux/amd64``
    and ``linux/arm64`` (version from the tag): a tarball with the static
    binary, the READMEs and the systemd example, and a .deb built with fpm
    (installed with ``gem install fpm -v 1.18.0``). A second job, in the
    ``release`` environment with ``contents: write``, creates the GitHub
    release with ``gh release create --verify-tag --generate-notes`` and
    uploads tarballs, packages and ``SHA256SUMS``. Tags containing ``-``
    become pre-releases, and a re-run replaces the files of an existing
    release.

Makefile and Debian package
---------------------------

The ``Makefile`` wraps ``build``, ``test``, ``lint``, ``dist``, ``deb``,
``checksums`` and ``docker``. ``GOARCH`` selects the target architecture;
``VERSION`` defaults to ``git describe`` of the latest ``v*`` tag.

``make deb`` follows opensmtpd-filter-greylistd's fpm recipe (``-s dir -t
deb``, same maintainer, ``Depends: greylistd``). Files are staged with
explicit modes and installed under ``/usr/local/greylistd-frontend``:
``bin/greylistd-frontend``, ``example/`` (unit and environment file) and
``README.rst``. The package activates nothing; the example unit's
``ExecStart`` points at the package's binary path, so it works as shipped.
Debian versions map ``-`` to ``~`` (``1.3.0-rc1`` → ``1.3.0~rc1``, sorting
before ``1.3.0``) and post-tag builds to ``+gitN.SHA``; the iteration defaults
to ``0`` as in filter-greylistd. Installing to ``/usr/local`` from a package
is against Debian policy (§9.1.2) but was asked for explicitly, keeping
everything of this service in one directory.

* ``dependabot.yml``: weekly, grouped updates for gomod (minor/patch) and
  github-actions, plus the Dockerfile's base image (docker), because a gomod
  update can raise the ``go`` line in ``go.mod`` beyond the build image's Go
  version.


Security summary
================

* **Headers:** a strict CSP (``default-src 'self'``, no inline code,
  ``frame-ancestors 'none'``, ``form-action 'self'``), plus ``nosniff``,
  ``X-Frame-Options: DENY`` and ``Referrer-Policy: same-origin``.
* **Requests:** CSRF tokens on every state change, and ``SameSite=Lax``
  cookies. The ``back`` redirect is rebuilt and kept local.
* **Input to greylistd:** validated against a whitelist of characters, and
  option injection is blocked.
* **Login:** a rate limit on password logins, and constant-time
  comparisons. OIDC requires an allow list by default, and anonymous access
  must be opted into explicitly.
* **Container:** a minimal image, running without root, with a read-only
  root filesystem and no capabilities (in the Nomad example).


Assumptions
===========

1. **Placement:** the frontend runs on the same host as greylistd and
   reaches it only through the socket. No TCP and no ``greylist`` CLI.
2. **Data source:** greylistd keeps the triplet text
   (``savetriplets = true``, the default). Entries without retained text
   cannot be listed by greylistd and so do not appear in the UI.
3. **Time zones:** greylistd's timestamps are in the host's local time, and
   the operator sets ``--timezone`` to match. Times are shown in that zone.
4. **What gets whitelisted:** domains and IPs are what users add.
   filter-greylistd checks the eTLD+1 and eTLD+2 of the rDNS name, then
   the SPF-validated sender domain or the IP. The UI's help text explains
   this. Triplets are listed and can be moved or deleted, but the add form
   is aimed at single tokens.
5. **Users:** a single admin or a handful of them. Therefore:

   * there is no server-side session store and no per-user audit beyond
     the application log;
   * rate limiter state lives in memory per process.

6. **TLS:** a reverse proxy terminates TLS in production. ``--base-url``
   decides whether cookies are ``Secure``, and ``--trust-proxy`` whether
   ``X-Forwarded-For`` is trusted. Only ``X-Forwarded-For`` is used; there
   is no ``X-Forwarded-Proto`` handling.
7. **OIDC client:** a confidential client with a client secret is assumed.
   This matched the test IdP's "Authorization" client type, so a hybrid
   client was not needed.
8. **Registry:** ghcr.io is the default, and the GitHub repository is
   ``jdelic/greylistd-frontend`` (module path and image placeholder),
   matching the author's other projects.
9. **Branches:** the default branch is ``master``. CI triggers on it.
   Adjust if the GitHub repository uses ``main``.
10. **Socket owner:** uid/gid 108 for ``opensmtpd`` is specific to the VM.
    The Nomad example says to check it with ``id opensmtpd``.


Changes from the conversation
=============================

In order:

1. **Test environment (user):** a mail user ``jonas@maurusnet.test`` was
   provided for the relay and for OIDC at ``https://auth.maurusnet.test/o2``.
   Registering an OIDC client myself failed (no DB access), so the user
   registered ``greylistd-frontend`` with type "Authorization" and the
   redirect URI ``http://localhost:8080/auth/oidc/callback``.

   * The client secret is kept only in the session scratchpad and is never
     committed.

2. **Screenshots (user):** screenshots are shared via artifactsd's ``temp``
   collection (``artifacts.maurus.net/c/temp/greylistd-frontend/…``). Taking
   them uncovered UI fixes:

   * list badges wrapped one letter per line on desktop
     (``white-space: nowrap``);
   * the mobile top bar wrapped the Logout button (the user name is now
     hidden below 520px);
   * validation errors repeated the value, so the messages were shortened;
   * the ``--trust-proxy`` help mentioned ``X-Forwarded-Proto``, which is
     not used, and was corrected.

3. **GitHub Actions (user):** build and push on tags to a registry
   configured via variables and secrets; lint and build ("linting, not
   linking", clarified by the user) on master and PRs; Dependabot for Go
   and Actions. Lint fixes that followed:

   * golangci-lint findings fixed: 7 errcheck and 1 QF1001;
   * gosec findings G124 (Secure cookie) and G710 (redirect) are justified
     inline;
   * shellcheck SC2034 in the smoke test fixed.

4. **Commit (user approved):** everything was committed to ``master`` as
   063c0cb, authored with the user's GitHub noreply identity (none was
   configured on the VM). ``.agent-docs/`` was left untracked.
5. **Login modes (user):**

   * "If there is no local login configured, the user is instantly
     redirected to the OpenID login": implemented, with the logout and
     error exceptions above to avoid loops.
   * "If no login is configured a flag should allow access for all users,
     otherwise display an error message":

     * added ``--allow-anonymous``;
     * "no login method" was previously a startup error; the server now
       starts and shows the error in the UI (503);
     * sessions are now bound to their login method being enabled;
     * README and tests updated.


Verification
============

Unit and handler tests (``go test -race ./...``) cover:

* the socket protocol, against a fake UNIX socket server;
* config precedence and validation;
* cookie signing, tampering, purpose separation and expiry;
* the OIDC allow logic;
* login: login required, wrong password and login CSRF, the rate limiter,
  and the login modes (OIDC-only redirect, no login, anonymous including
  CSRF and session invalidation);
* entries: add, row and bulk actions, filter and sort, and local-only back
  redirects.

Manually verified on the VM against the real greylistd and IdP, scripted
with chromedp and Chromium:

* password and OIDC login, including the PKCE/nonce flow and the allow
  list;
* add, move, delete and bulk operations;
* the OIDC-only redirect, logout and re-login;
* the no-login error page and anonymous mode.

The container was verified as uid 108 with the socket mounted, and the
arm64 cross-build was checked.


Known limitations and possible follow-ups
=========================================

* **Logout:** stateless sessions cannot be revoked on the server. Logout
  deletes the cookie, but a copied cookie stays valid until it expires.
  Rotating ``--session-secret`` invalidates all sessions.
* **No IdP logout:** RP-initiated logout (``end_session_endpoint``) is not
  implemented, so logging out leaves the IdP session alive. That is why the
  OIDC-only redirect is suppressed after logout.
* **Rate limiter:** it is per process and lost on restart. Fine for a
  single instance.
* **No pagination:** greylistd lists are small on a personal mail server.
  The whole list is fetched on each page load and filtered and sorted in
  memory.
* **No hit details:** greylistd's per-entry history is not exposed beyond
  last seen and count.
* **Workflows untested on GitHub:** they have only been checked locally
  (actionlint and an equivalent smoke script) until the repository is
  pushed.
