# sa_dump.nim — golden-fixture harness for state-actor's internal/nimbus.
#
# Initialises a fresh persistent nimbus database from a genesis JSON through
# the client's own genesis path (CommonRef.new → writeGenesis + initializeDb),
# then reopens the RocksDB read-only and dumps every (key, value) of the
# AriVtx and KvtGen column families as hex JSON, sorted by key.
#
# Build inside a nimbus-eth1 checkout at the pinned commit (see README.md):
#
#   cp sa_dump.nim <nimbus-eth1>/tools/sa_dump/sa_dump.nim
#   cd <nimbus-eth1> && ./env.sh nim c -d:release --out:build/sa_dump tools/sa_dump/sa_dump.nim
#   build/sa_dump genesis.json /tmp/sa-nimbus-datadir /tmp/genesis_dump.json
#
# Output shape (matches internal/ethrex/testdata/genesis_dump.json):
#   {"cfs": {"AriVtx": [["0x<key>", "0x<value>"], ...], "KvtGen": [...]}}
# The AriVtx admin record has the empty key and is emitted as "0x".

import
  std/[algorithm, os, strutils],
  results,
  stew/byteutils,
  rocksdb,
  ../../execution_chain/db/opts,
  ../../execution_chain/db/core_db/persistent,
  ../../execution_chain/common/[common, chain_config_loader]

const dumpedCfs = ["AriVtx", "KvtGen"]

proc toHex0x(b: openArray[byte]): string =
  "0x" & b.toHex().toLowerAscii()

proc dumpCf(db: RocksDbReadOnlyRef, name: string): seq[(seq[byte], seq[byte])] =
  let cf = db.getColFamily(name).expect("column family " & name)
  let it = db.openIterator(cfHandle = cf.handle).expect("iterator for " & name)
  for key, value in it.pairs():
    result.add((key, value))
  result.sort(proc(a, b: (seq[byte], seq[byte])): int = cmp(a[0], b[0]))

proc main() =
  if paramCount() != 3:
    stderr.writeLine "usage: sa_dump <genesis.json> <datadir> <out.json>"
    quit 1
  let
    genesisPath = paramStr(1)
    dataDir = paramStr(2)
    outPath = paramStr(3)

  var params: NetworkParams
  if not loadNetworkParams(genesisPath, params):
    stderr.writeLine "cannot load " & genesisPath
    quit 1

  block init:
    # wipe = true: start from an empty <dataDir>/ecdb. CommonRef.new runs the
    # genesis alloc + header + initializeDb persist, exactly as a first boot.
    let db = newCoreDbRef(AristoDbRocks, dataDir, DbOptions.init(), wipe = true)
    let com = CommonRef.new(db, params)
    stderr.writeLine "genesis " & $com.genesisHeader.number & " state root " &
      $com.genesisHeader.stateRoot
    db.close()

  let ecdb = dataDir / "ecdb"
  let onDisk = listColumnFamilies(ecdb).expect("listColumnFamilies")
  var descs: seq[ColFamilyDescriptor]
  for name in onDisk:
    descs.add initColFamilyDescriptor(name, defaultColFamilyOptions(autoClose = true))
  let ro = openRocksDbReadOnly(ecdb, columnFamilies = descs).expect("openRocksDbReadOnly")
  defer: ro.close()

  var buf = "{\n  \"cfs\": {\n"
  for i, name in dumpedCfs:
    buf.add "    \"" & name & "\": [\n"
    let rows = dumpCf(ro, name)
    for j, row in rows:
      buf.add "      [\n        \"" & row[0].toHex0x() & "\",\n        \"" &
        row[1].toHex0x() & "\"\n      ]"
      buf.add(if j + 1 < rows.len: ",\n" else: "\n")
    buf.add "    ]"
    buf.add(if i + 1 < dumpedCfs.len: ",\n" else: "\n")
    stderr.writeLine name & ": " & $rows.len & " rows"
  buf.add "  }\n}\n"
  writeFile(outPath, buf)

main()
