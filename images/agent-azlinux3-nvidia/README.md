# agent-azlinux3-nvidia

Base Azure Linux 3.0 container image used to build the rootfs for unbounded-agent nspawn machines with NVIDIA GPU support.

This image extends the base `agent-azlinux3` image with the
[NVIDIA Container Toolkit](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/install-guide.html)
pre-installed, enabling containers to access host NVIDIA GPU devices.

Supports both amd64 and arm64 architectures.
