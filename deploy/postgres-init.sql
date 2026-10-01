-- Local development only. One database per service, and two roles per
-- service: an owner that runs migrations and an application role with the
-- minimum grants. Production creates these through infrastructure as code
-- with generated credentials.
CREATE ROLE ledger_owner   LOGIN PASSWORD 'devpassword';
CREATE ROLE ledger_app     LOGIN PASSWORD 'devpassword';
CREATE ROLE identity_owner LOGIN PASSWORD 'devpassword';
CREATE ROLE identity_app   LOGIN PASSWORD 'devpassword';
CREATE ROLE lending_owner  LOGIN PASSWORD 'devpassword';
CREATE ROLE lending_app    LOGIN PASSWORD 'devpassword';

CREATE DATABASE ledger   OWNER ledger_owner;
CREATE DATABASE identity OWNER identity_owner;
CREATE DATABASE lending  OWNER lending_owner;
