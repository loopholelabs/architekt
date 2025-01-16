sudo drafter-mounter --laddr '' --raddr 'localhost:1337' --devices '[
  {
    "name": "testdata",
    "base": "testdata2",
    "blockSize": 1048576,
    "expiry": 1000000000,
    "maxDirtyBlocks": 200,
    "minCycles": 5,
    "maxCycles": 20,
    "cycleThrottle": 500000000,
    "makeMigratable": true,
    "shared": false
  }
]'
