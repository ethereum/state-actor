//! Regeneration harness for state-actor's ethrex golden fixture.
//! Builds a genesis store via the real `add_initial_state` path, then dumps
//! every column family as hex JSON so the Go codec can be golden-validated
//! against testdata/genesis_dump.json (see README.md for how to run it).
//!
//! Run: cargo run -p ethrex-storage --features rocksdb --example sa_dump -- \
//!        <genesis.json> <datadir> <out.json>

use std::collections::BTreeMap;
use std::fs::File;
use std::io::BufReader;

use ethrex_common::types::Genesis;
use ethrex_storage::api::tables::TABLES;
use ethrex_storage::api::StorageBackend;
use ethrex_storage::backend::rocksdb::RocksDBBackend;
use ethrex_storage::{DEFAULT_ROCKSDB_BLOCK_CACHE_SIZE_BYTES, EngineType, Store};

fn hex(b: &[u8]) -> String {
    let mut s = String::with_capacity(2 + b.len() * 2);
    s.push_str("0x");
    for byte in b {
        s.push_str(&format!("{byte:02x}"));
    }
    s
}

#[tokio::main]
async fn main() {
    let args: Vec<String> = std::env::args().collect();
    let genesis_path = &args[1];
    let datadir = &args[2];
    let out_path = &args[3];

    // 1. Write genesis through the real ethrex genesis path.
    let file = File::open(genesis_path).expect("open genesis");
    let genesis: Genesis = serde_json::from_reader(BufReader::new(file)).expect("parse genesis");
    let genesis_block = genesis.get_block();
    let genesis_hash = genesis_block.hash();
    let genesis_state_root = genesis_block.header.state_root;

    {
        let mut store = Store::new(datadir, EngineType::RocksDB).expect("open store");
        store.add_initial_state(genesis).await.expect("add_initial_state");
        // Drop store to release the RocksDB lock and stop the FKV generator.
    }
    // Give the background FKV generator thread time to exit (it should not have
    // populated anything yet, but we want a clean read).
    std::thread::sleep(std::time::Duration::from_millis(200));

    // 2. Reopen read-only via the backend and dump every CF.
    let backend = RocksDBBackend::open(datadir, DEFAULT_ROCKSDB_BLOCK_CACHE_SIZE_BYTES)
        .expect("reopen backend");
    let read = backend.begin_read().expect("begin_read");

    let mut dump: BTreeMap<String, Vec<[String; 2]>> = BTreeMap::new();
    let mut counts: BTreeMap<String, usize> = BTreeMap::new();
    for table in TABLES {
        let iter = read.prefix_iterator(table, &[]).expect("prefix_iterator");
        let mut rows = Vec::new();
        for item in iter {
            let (k, v) = item.expect("row");
            rows.push([hex(&k), hex(&v)]);
        }
        counts.insert(table.to_string(), rows.len());
        dump.insert(table.to_string(), rows);
    }

    let out = serde_json::json!({
        "genesis_hash": hex(genesis_hash.as_bytes()),
        "genesis_state_root": hex(genesis_state_root.as_bytes()),
        "counts": counts,
        "cfs": dump,
    });
    std::fs::write(out_path, serde_json::to_vec_pretty(&out).unwrap()).expect("write out");
    eprintln!("genesis_hash       = {}", hex(genesis_hash.as_bytes()));
    eprintln!("genesis_state_root = {}", hex(genesis_state_root.as_bytes()));
    eprintln!("wrote {out_path}");
}
