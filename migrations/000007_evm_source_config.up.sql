-- Values of sources.config per network (SRS - EVM Connector §3.1 "Values per network", §2.4 Seed data; S3 D-17)
-- and the alias of the tracked token of base-sepolia (S3 D-27). Merged into the existing config: chain_id and a
-- treasury_connection written before stay. Defaults are not written. The values of anvil that depend on its
-- deployment are set with casctl source set.

UPDATE sources SET config = config || '{"finality_mode": "confirmations", "finality_confirmations": 10}'
WHERE code = 'anvil';

UPDATE sources SET config = config || '{
    "finality_mode": "tag",
    "finality_tag": "finalized",
    "controller_address": "0xF75D58dc6E33487dB994D81D0d870D61Eac45F37",
    "backfill_floor": 47768907
}'
WHERE code = 'base-sepolia';

-- MockUSDC of base-sepolia, in EIP-55 form.
INSERT INTO asset_aliases (source_id, native_asset, asset, decimals)
SELECT id, '0x6c0434c821694513FFfd5364D63f27F05d73f3aB', 'USDC', 6 FROM sources WHERE code = 'base-sepolia';
