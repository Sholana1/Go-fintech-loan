# Identity gRPC contract (`bank.identity.v1`)

Definition: [proto/bank/identity/v1/identity.proto](../../proto/bank/identity/v1/identity.proto). Internal only; customers use identity's REST endpoints.

| RPC | Purpose | Allowed callers | Retry |
|---|---|---|---|
| `GetCustomer` | KYC view: status, tier, name, date of birth, deposit account code. Never the BVN. | `lending` | Safe on `UNAVAILABLE`, `DEADLINE_EXCEEDED` |
| `VerifyCustomerPin` | Step-up check. Counts failed attempts under a row lock. | `lending` | **Do not retry**: a retry may consume an attempt |
| `GetCreditBureauSubject` | BVN, name, date of birth for a consented bureau enquiry. Requires `consent_ref`. Every call writes an audit record in the same transaction as the read. | `lending` | Safe |

| gRPC status | Meaning |
|---|---|
| `NOT_FOUND` | Unknown customer |
| `PERMISSION_DENIED` | Caller's workload identity is not allowed this RPC |
| `INVALID_ARGUMENT` | Malformed id or missing consent reference |
| `INTERNAL` | Logged, not returned |

## REST (customers and staff)

| Method and path | Purpose |
|---|---|
| `POST /v1/customers` | Register: phone, full name, date of birth, BVN, PIN. The BVN is verified before anything is stored. |
| `POST /v1/sessions` | Phone + PIN → access token. Five wrong PINs lock the credential for 15 minutes. |
| `POST /v1/staff/sessions` | Development-only staff login. Production staff authentication is SSO (launch dependency). |
