-- Rights of the role cas_card_auth (SRS - Core §3.2). The operator creates the role once per environment.
-- The card tables and their rights come with the card schema.

-- /readyz compares the schema version with the one the binary expects.
GRANT SELECT ON schema_migrations TO cas_card_auth;
