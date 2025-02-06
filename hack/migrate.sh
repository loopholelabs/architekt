sudo drafter-peer --netns ark1 --raddr 'localhost:1337' --laddr '' --devices '[
  {
    "name": "state",
    "base": "image/instance-1/state.bin",
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
    "base": "image/instance-1/memory.bin",
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
    "base": "image/instance-1/vmlinux",
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
    "base": "image/instance-1/rootfs.ext4",
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
    "base": "image/instance-1/config.json",
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
    "base": "image/instance-1/oci.ext4",
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
