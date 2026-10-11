-- Wrong passwords at sign-in are limited the way wrong passcodes are (R-404,
-- issue #189), and counted in the same table, which is renamed for it. A key
-- says what it counts: "app_…|<address>" a passcode guess (R-075a),
-- "signin|user|<username>" and "signin|addr|<address>" a password.
ALTER TABLE passcode_failures RENAME TO failed_attempts;
ALTER INDEX passcode_failures_key RENAME TO failed_attempts_key;
