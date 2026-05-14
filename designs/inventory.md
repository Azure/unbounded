# Inventory

The inventory subsystem discovers hardware on bare-metal nodes and aggregates
the data into a central PostgreSQL database for browsing and analysis.

## Components

| Component | Binary | Description |
|-----------|--------|-------------|
| **Agent** | `inventory-agent` | Runs on each node, collects hardware inventory, stores it locally in SQLite, and publishes it to the aggregator over gRPC. |
| **Aggregator** | `inventory-aggregator` | Central gRPC server that receives inventory submissions from agents and persists them to PostgreSQL. |
| **Viewer** | `inventory-viewer` | Read-only web UI and JSON API for browsing the aggregated inventory. |
| **Inspector** | `inventory-inspector` | Periodic analysis of inventory data (not yet implemented). |

## Data Flow

```text
+------------------+         gRPC (SubmitInventory)        +----------------------+
|  inventory-agent | -------------------------------------> | inventory-aggregator |
|  (per node)      |                                       |  (central)           |
+------------------+                                       +----------------------+
        |                                                          |
        v                                                          v
   local SQLite                                               PostgreSQL
   (node cache)                                                    |
                                                       +-----------+-----------+
                                                       |                       |
                                                       v                       v
                                              inventory-viewer       inventory-inspector
                                              (HTTP / web UI)         (CronJob, stub)
```

The agent writes inventory data both locally (SQLite) and remotely (gRPC to the
aggregator). The local SQLite copy serves as a cache on the node. The viewer and
inspector read from the shared PostgreSQL database.

## Agent

The agent runs as root on each node and collects hardware inventory from sysfs,
procfs, and system utilities.

### Collected Device Types

| Type | Data Source |
|------|-------------|
| `chassis` | DMI sysfs (`/sys/class/dmi/id`), fallback to `product_uuid` or `/etc/machine-id` |
| `bmc` | IPMI via `/dev/ipmi0` |
| `cpu` | `/proc/cpuinfo` and `lscpu` |
| `memory` | `/proc/meminfo` and `dmidecode` (DIMM info) |
| `disk` | `/sys/block/*` and disk serial/firmware utilities |
| `nic` | `/sys/class/net/*` (physical NICs only) and `ethtool` |
| `gpu` | PCI bus scan (`/sys/bus/pci/devices`), enriched with `lspci` and `nvidia-smi` |

### Collected Neighbor Types

| Type | Data Source |
|------|-------------|
| LLDP neighbors | `lldpctl -f json` (network topology discovery) |
| IMEX/NVLink peers | `nvidia-imex-ctl -H` (GPU fabric topology) |

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--debug` | `false` | Enable debug output |
| `--db` | `./inventory.db` | Path to local SQLite database file |
| `--collector` | `inventory-collector:50051` | gRPC address of the aggregator |

### Required CLI Utilities

The agent reads most data directly from sysfs and procfs but shells out to
several utilities for information that is not available through those
filesystems. All utilities are optional - the agent collects what it can and
skips data that requires a missing tool.

| Utility | Package (Ubuntu/Debian) | Used For |
|---------|------------------------|----------|
| `dmidecode` | `dmidecode` | Per-DIMM memory details and CPU serial numbers (SMBIOS) |
| `ethtool` | `ethtool` | NIC firmware version |
| `lldpctl` | `lldpd` | LLDP network neighbor discovery |
| `lspci` | `pciutils` | PCI device enumeration for GPU discovery |
| `modinfo` | `kmod` | Kernel module versions for disk, GPU, and NIC drivers |
| `nvidia-imex-ctl` | NVIDIA IMEX package | NVLink/IMEX multi-node GPU fabric topology |
| `nvidia-smi` | NVIDIA driver package | Detailed GPU info (serial, VRAM, VBIOS, driver version) |
| `udevadm` | `udev` / `systemd` | Disk serial numbers |

For a minimal deployment, `dmidecode`, `ethtool`, `lspci`, `modinfo`, and
`udevadm` cover the core hardware inventory. The NVIDIA and LLDP utilities are
only needed on nodes with GPUs or LLDP-capable switches respectively.

## Aggregator

The aggregator opens a PostgreSQL connection, ensures the schema exists, and
starts a gRPC server that implements the `InventoryAggregator` service.

Devices are upserted by `serial_number`. Neighbors are upserted by the
composite key `(host_identifier, local_interface)`. In both cases an update
only occurs when the data has actually changed.

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--debug` | `false` | Enable debug output |
| `--grpc-addr` | `:50051` | gRPC listen address |

### Environment Variables (PostgreSQL)

| Variable | Description |
|----------|-------------|
| `POSTGRES_HOST` | Database hostname |
| `POSTGRES_PORT` | Database port |
| `POSTGRES_DB_NAME` | Database name |
| `POSTGRES_USER` | Database user |
| `POSTGRES_PASSWORD` | Database password |
| `POSTGRES_SSL_MODE` | SSL mode (`disable`, `require`, `verify-full`, etc.) |

## Viewer

The viewer serves a read-only web UI and JSON API over HTTP.

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--addr` | `:8080` | HTTP listen address |

### HTTP Endpoints

| Method | Path | Description |
|--------|------|-------------|
| GET | `/healthz` | Health check |
| GET | `/api/devices` | All device records as JSON |
| GET | `/api/neighbors` | All neighbor records as JSON |
| GET | `/` | Embedded single-page web UI |

The viewer uses the same PostgreSQL environment variables as the aggregator.

## Inspector

The inspector is intended to run as a Kubernetes CronJob for periodic analysis
of the inventory data. It is not yet implemented.

## gRPC API

Defined in `api/inventory/v1/inventory.proto`.

**Service:** `InventoryAggregator`

| RPC | Request | Response |
|-----|---------|----------|
| `SubmitInventory` | `SubmitInventoryRequest` | `SubmitInventoryResponse` |

**Messages:**

| Message | Fields |
|---------|--------|
| `DeviceRecord` | `device_type`, `device_name`, `host_identifier`, `serial_number`, `attributes` (JSON bytes) |
| `NeighborRecord` | `host_identifier`, `local_interface`, `attributes` (JSON bytes) |
| `SubmitInventoryRequest` | `repeated DeviceRecord devices`, `repeated NeighborRecord neighbors` |
| `SubmitInventoryResponse` | `devices_saved` (int32), `neighbors_saved` (int32) |

## Database Schema

The agent's local SQLite and the aggregator's PostgreSQL share the same logical
schema.

### `inventory` table

| Column | Type | Constraints |
|--------|------|-------------|
| `id` | auto-increment | PRIMARY KEY |
| `device_type` | TEXT | NOT NULL |
| `device_name` | TEXT | NOT NULL |
| `host_identifier` | TEXT | NOT NULL |
| `serial_number` | TEXT | NOT NULL, UNIQUE |
| `attributes` | TEXT | NOT NULL (JSON) |

### `neighbors` table

| Column | Type | Constraints |
|--------|------|-------------|
| `id` | auto-increment | PRIMARY KEY |
| `host_identifier` | TEXT | NOT NULL |
| `local_interface` | TEXT | NOT NULL |
| `attributes` | TEXT | NOT NULL (JSON) |
| | | UNIQUE(`host_identifier`, `local_interface`) |

Attributes are schema-less JSON. Each device type defines its own Go struct that
is serialized to JSON before storage, keeping the database schema uniform across
all device types.

## Deployment

Kubernetes manifests live in `deploy/inventory/`. PostgreSQL is assumed to be
provisioned separately.

| Manifest | Resource |
|----------|----------|
| `common/01-namespace.yaml` | Namespace `inventory` |
| `common/02-config.yaml` | ConfigMap with PostgreSQL connection settings |
| `common/03-secret.yaml` | Secret with `POSTGRES_PASSWORD` |
| `collector/01-deployment.yaml` | Deployment for the aggregator (includes init container to create the database) |
| `collector/02-service.yaml` | ClusterIP service exposing gRPC port 50051 |
| `inspector/01-cronjob.yaml` | CronJob running the inspector hourly |
| `viewer/01-deployment.yaml` | Deployment for the web viewer |

## Building

```sh
# Build all inventory components
make inventory-all

# Build individual components
make inventory-agent
make inventory-aggregator
make inventory-inspector
make inventory-viewer

# Build container images
make inventory-oci-all
```
