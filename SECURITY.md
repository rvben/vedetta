# Security policy

Vedetta handles private video, camera credentials, network access, and
long-lived recordings. Security reports are taken seriously.

## Supported versions

Security fixes are provided for the latest released version. When practical,
the fix may also be backported to the immediately preceding minor release.
Development snapshots are not supported releases.

## Reporting a vulnerability

Do not open a public issue for a suspected vulnerability. Use GitHub's private
vulnerability reporting feature on the repository's Security tab. If private
reporting is unavailable, contact the repository owner through the private
contact method listed on their GitHub profile and ask for a secure reporting
channel without including exploit details in the first message.

Include, when available:

- affected version or commit;
- deployment and exposure mode;
- prerequisites and required privileges;
- reproducible steps or a minimal proof of concept;
- impact on confidentiality, integrity, or availability;
- suggested remediation.

Please remove credentials, tokens, private URLs, identifiable footage, and
unrelated personal information. Allow reasonable time for validation and a
coordinated fix before disclosure.

## Security boundaries

The authenticated HTTP port, RTSP republish server, proxy-auth configuration,
MQTT broker, WebRTC ICE servers, notification endpoints, model files, and
camera-provided RTSP/ONVIF data are security boundaries. A trusted LAN is not a
substitute for authentication. Use TLS or a trusted reverse proxy when traffic
crosses an untrusted network.

Dependencies and bundled artifacts must have an attributable upstream source,
pinned version, integrity verification where feasible, and an update path.
