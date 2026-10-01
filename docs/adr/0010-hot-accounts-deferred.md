# 0010: System-account striping deferred, with measurements

**Plan.** Stripe hot system accounts so one row lock does not serialise all postings.

**Measured** (laptop, 8 cores, PostgreSQL 16 in Docker, provider simulator; `make bench`):

| Concurrency | Mean ledger posting time (disbursement) |
|---|---|
| 1 | 7.4 ms |
| 16 | 99 ms |

Every disbursement locks `SYS:LOANS_PRINCIPAL` and `SYS:FEE_INCOME`. At concurrency 16 postings queue on those two rows, as predicted. The implied ceiling on this machine is about 135 postings per second through one hot row.

**Decision.** Do not stripe yet. The growth scenario has about 65,000 loan disbursements and repayments a day, a peak of a few per second. One hot row has roughly fifty times that headroom here.

**Revisit** with transfers, where fee, tax and clearing accounts are touched by every payment and the planned burst is ~390 TPS. The measurement to repeat is lock wait on system-account balance rows at twice the planned burst.
