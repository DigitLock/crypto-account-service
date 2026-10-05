-- EVM networks of SRS - EVM Connector §2.4 Seed data: enabled, with chain_id only.

INSERT INTO sources (code, kind, enabled, config) VALUES
    ('anvil', 'EVM', true, '{"chain_id": 31337}'),
    ('base-sepolia', 'EVM', true, '{"chain_id": 84532}');
