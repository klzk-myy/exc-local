-- 027_alter_users_add_password_hash.down.sql — reverse of the paired
-- ALTER (drops exactly the columns the up migration adds).

BEGIN;

ALTER TABLE users
    DROP COLUMN password_hash,
    DROP COLUMN country,
    DROP COLUMN full_name,
    DROP COLUMN address,
    DROP COLUMN email_verified_at,
    DROP COLUMN totp_backup_codes;

ALTER TABLE users ALTER COLUMN totp_secret TYPE VARCHAR(64);

COMMIT;
