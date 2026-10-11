DELETE FROM failed_attempts WHERE key LIKE 'signin|%';
ALTER INDEX failed_attempts_key RENAME TO passcode_failures_key;
ALTER TABLE failed_attempts RENAME TO passcode_failures;
