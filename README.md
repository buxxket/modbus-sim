[Modbus Simulator](github.com/techplexengineer/modbus-sim)
================

A simple modbus/tcp server based on the modbus server implementation from github.com/tbrandon/mbserver

Register exposure and values are fully configuration-driven via [registers.json](registers.json).

## Supported Architectures
Simply pulling `techplex/modbus-sim:latest` should retrieve the correct image for your arch.

The architectures supported by this image are:
| Architecture | Available |
| :----------: | :-------: |
| x86-64       | ✅        |
| arm64        | ✅        |
| armhf        | ✅        |


## Application Setup
The application can be accessed at tcp://yourhost:1502

## Register Exposure Configuration

The simulator reads register definitions from [registers.json](registers.json) at startup.

You can change which registers are exposed while the simulator is running by editing [registers.json](registers.json). The server checks for file changes and automatically reloads the configuration (up to once per second).

When a requested register is not exposed by config, the simulator responds with `IllegalDataAddress`.

### Config format

```json
{
  "holding_registers": [
    { "register": 40100, "type": "uint16", "value": 65280 },
    { "register": 40500, "type": "float32", "value": 3.1415927 }
  ],
  "input_registers": [
    { "register": 30100, "type": "uint16", "value": 65280 },
    { "register": 30101, "type": "uint16", "value": 123 }
  ]
}
```

- `holding_registers` and `input_registers` are separate blocks for each register type.
- In these blocks, `register` must use full notation (`4xxxx` for holding, `3xxxx` for input).
- `type` supports: `uint16`, `int16`, `uint32`, `int32`, `float32`.
- `value` is encoded according to `type`.
- `uint32`, `int32`, and `float32` consume two register words starting at `register`.
- Requests that span any register outside configured entries are rejected.

Backward compatibility:
- A flat `registers` array is still supported.
- In `registers`, use full notation (`3xxxx` or `4xxxx`) to indicate register type.

### Optional config path override

Set `REGISTER_CONFIG_PATH` to load config from a different file:

```bash
REGISTER_CONFIG_PATH=/path/to/registers.json ./modbus-sim
```

## Usage
Here are some example snippets to help you get started creating a container.

### docker-compose

```yaml
---
version: "2.1"
services:
  modbus:
    image: techplex/modbus-sim:latest
    container_name: modbus
    ports:
      - 1502:1502
    restart: unless-stopped
```

### docker cli

```bash
docker run -d \
  --name=modbus \
  -p 1502:1502 \
  --restart unless-stopped \
  techplex/modbus-sim:latest
```

## Updating Info

Below are the instructions for updating containers:

### Via Docker Compose

* Update all images: `docker-compose pull`
  * or update a single image: `docker-compose pull techplex/modbus-sim`
* Let compose update all containers as necessary: `docker-compose up -d`
  * or update a single container: `docker-compose up -d techplex/modbus-sim`
* You can also remove the old dangling images: `docker image prune`

### Via Docker Run

* Update the image: `docker pull techplex/modbus-sim:latest`
* Stop the running container: `docker stop techplex/modbus-sim`
* Delete the container: `docker rm techplex/modbus-sim`
* Recreate a new container with the same docker run parameters as instructed above
* You can also remove the old dangling images: `docker image prune`

## Building locally

If you want to make local modifications to these images for development purposes or just to customize the logic:

```bash
git clone https://github.com/techplexengineer/modbus-sim.git
cd modbus-sim
docker build \
  --no-cache \
  --pull \
  -t techplex/modbus-sim:latest .
