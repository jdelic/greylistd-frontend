The context
===========

On my mailserver setup, I run Debian 13 with backports.org opensmtpd. I run it
with my own greylisting filter filter-greylistd which you can find in
`~/go/src/github.com/jdelic/opensmtpd-filter-greylistd` that uses greylistd
as a backend.

This setup runs on your localhost (see also `/etc/smtpd.conf`).

greylistd is also installed locally.

When I want to receive an email quickly, I sometimes have to log into my
server via SSH and run `greylist add` to enable a domain. In the future I would
like to do so via a small web interface running on my mailserver.

You're running on a testing VM under vagrant, so you can connect to the local
opensmtpd instance and greylistd and test as much as you want.


The job
=======

Create a Go program that provides a http server on a port and can run in a 
`FROM SCRATCH` Docker container. It should be able to directly communicate
with greylistd via its socket and

1. list all currently greylisted, whitelisted, or blocked domains in a 
   sortable and filterable list
2. Allow, via buttons, and in bulk via selection to whitelist or blacklist
   entries.
3. Have a simple interface to add new domains.
4. Support login via OpenID Connect
5. Support an optional alternative login via a username and password passed via
   environment variable.

All configuration can be passed via command-line or 12factor-compatible via
environment variables.

Access logs go stdout, application logs go to stderr and should be structured
JSON.

Provide an example Hashicorp Nomad configuration.

The web UI must be mobile-first, but browser-friendly. Design-wise it should
follow the same design language as artifactsd which is a separate project that
can be found in `~/artifactsd`. Use it only as a design reference.

