DROP TABLE cluster_signals;
DROP TABLE secrets_canary;
DROP TABLE passcode_failures;
ALTER TABLE detections DROP COLUMN replica_id;
ALTER TABLE deployments DROP COLUMN replica_id;
DROP TABLE pando_replicas;
