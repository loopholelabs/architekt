sudo drafter-peer --metrics ':2113' --netns ark1 --raddr 'localhost:1337' --laddr '' --devices '[
  {
    "name": "state",
    "base": "image/package/state.bin",
    "overlay": "image/instance-1/overlay/state.bin",
    "state": "image/instance-1/state/state.bin",
    "blockSize": 65536,
    "expiry": 1000000000,
    "maxDirtyBlocks": 200,
    "minCycles": 5,
    "maxCycles": 20,
    "cycleThrottle": 500000000,
    "makeMigratable": true,
    "shared": false
  },
  {
    "name": "memory",
    "base": "image/package/memory.bin",
    "overlay": "image/instance-1/overlay/memory.bin",
    "state": "image/instance-1/state/memory.bin",
    "blockSize": 65536,
    "expiry": 1000000000,
    "maxDirtyBlocks": 200,
    "minCycles": 5,
    "maxCycles": 20,
    "cycleThrottle": 500000000,
    "makeMigratable": true,
    "shared": false
  },
  {
    "name": "kernel",
    "base": "image/package/vmlinux",
    "overlay": "image/instance-1/overlay/vmlinux",
    "state": "image/instance-1/state/vmlinux",
    "blockSize": 65536,
    "expiry": 1000000000,
    "maxDirtyBlocks": 200,
    "minCycles": 5,
    "maxCycles": 20,
    "cycleThrottle": 500000000,
    "makeMigratable": true,
    "shared": false
  },
  {
    "name": "disk",
    "base": "image/package/rootfs.ext4",
    "overlay": "image/instance-1/overlay/rootfs.ext4",
    "state": "image/instance-1/state/rootfs.ext4",
    "blockSize": 65536,
    "expiry": 1000000000,
    "maxDirtyBlocks": 200,
    "minCycles": 5,
    "maxCycles": 20,
    "cycleThrottle": 500000000,
    "makeMigratable": true,
    "shared": false
  },
  {
    "name": "config",
    "base": "image/package/config.json",
    "overlay": "image/instance-1/overlay/config.json",
    "state": "image/instance-1/state/config.json",
    "blockSize": 65536,
    "expiry": 1000000000,
    "maxDirtyBlocks": 200,
    "minCycles": 5,
    "maxCycles": 20,
    "cycleThrottle": 500000000,
    "makeMigratable": true,
    "shared": false
  },
  {
    "name": "oci",
    "base": "image/package/oci.ext4",
    "overlay": "image/instance-1/overlay/oci.ext4",
    "state": "image/instance-1/state/oci.ext4",
    "blockSize": 65536,
    "expiry": 1000000000,
    "maxDirtyBlocks": 200,
    "minCycles": 5,
    "maxCycles": 20,
    "cycleThrottle": 500000000,
    "makeMigratable": true,
    "shared": false
  }
]'
