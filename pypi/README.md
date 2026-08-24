# Vedetta

Vedetta is a local-first network video recorder with recording, review,
multi-protocol live video, and real-time object detection. The application is
written in Go and does not require Python at runtime.

## Package status

This PyPI project is reserved for Vedetta. It does not currently install the
application. Use a release binary, Docker image, or source build instead:

```sh
# Docker
docker run -d --name vedetta --network host \
  -v vedetta-config:/config -v vedetta-data:/data \
  ghcr.io/rvben/vedetta:latest

# Build from source
git clone https://github.com/rvben/vedetta.git
cd vedetta && make build
```

Hardware-accelerated H.264 decoding is available through VideoToolbox on macOS
and through opt-in VA-API/NVDEC builds on Linux. Object inference currently
runs on CPU.

See the [GitHub repository](https://github.com/rvben/vedetta) for releases,
compatibility details, and documentation.
