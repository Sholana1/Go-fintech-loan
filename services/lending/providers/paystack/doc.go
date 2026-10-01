// Package paystack is a production adapter for Paystack's REST API. It
// implements two of lending's ports with real HTTP calls:
//
//	app.PayoutProvider    name enquiry, transfer, transfer status, transfer list
//	app.PaymentVerifier   verification of a payment a customer made to us
//
// # Where the contract comes from
//
// Every path, field and status used here was taken from Paystack's public
// OpenAPI specification (github.com/PaystackOSS/openapi, read 2026-10-01).
// Nothing was guessed. The endpoints used:
//
//	GET  /bank/resolve                  resolve.go     bank_resolveAccountNumber
//	POST /transferrecipient             transfer.go    transferrecipient_create
//	POST /transfer                      transfer.go    transfer_initiate
//	GET  /transfer/verify/{reference}   query.go       transfer_verify
//	GET  /transfer                      settlement.go  transfer_list
//	GET  /transaction/verify/{reference} collection.go transaction_verify
//
// # What has NOT been verified
//
//   - This adapter has never been run against Paystack. No credentials were
//     available and no live or sandbox call was made. Its tests run it
//     against a local HTTP server that returns the shapes the specification
//     describes. Before real use it must pass a sandbox certification run.
//   - The specification lists the transfer statuses but does not define
//     them. Only "success", "failed" and "reversed" are treated as final
//     here; every other status is treated as "not yet known" (status.go).
//   - Which error responses to POST /transfer guarantee that no transfer was
//     created is not stated in the specification. Every such response is
//     therefore treated as an UNKNOWN outcome and resolved by status query.
//   - Webhook signing (webhook.go) is not part of the OpenAPI specification.
//     It is implemented from Paystack's published documentation as recalled
//     and is disabled unless explicitly enabled.
//
// # Rules the adapter keeps (the same as every payment adapter here)
//
//   - Our reference is sent with the transfer; the same reference is used
//     for every later question about it.
//   - Doubt is never turned into an outcome. A timeout, a transport error, a
//     5xx, an undecodable body, a body about a different reference, or a
//     status this code does not know is returned as an error, which callers
//     treat as "unknown, ask again".
//   - Amounts are integer kobo end to end; Paystack's amount field is in
//     the currency's subunit, so no conversion happens.
//   - The secret key is sent only in the Authorization header and never
//     appears in an error, a log line or a URL.
package paystack
