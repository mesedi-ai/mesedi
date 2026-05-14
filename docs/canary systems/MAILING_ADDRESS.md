# Canary Systems, LLC — Mailing Address (Anytime Mailbox)

**Status:** Will use the same Anytime Mailbox CMRA address as Verdifax, LLC and (eventually) Mesedi, LLC. One physical mailbox, multiple authorized recipient entities — operationally clean, single Form 1583 filing per entity to add it as an authorized recipient.

---

## Address (for all forms requiring a mailing address)

```
Canary Systems, LLC
2903 West New Haven Ave, #684
West Melbourne, FL 32904
United States
```

This is a Commercial Mail Receiving Agent (CMRA) operated by Anytime Mailbox at the West Melbourne, FL location.

## Why this address — design rationale

- **CMRA-grade USPS registration** (registered with USPS via Form 1583 process) — accepted by every registrar, payment processor, and bank that doesn't have specific anti-CMRA filtering
- **Consistent with the founder's existing entities** — Verdifax, LLC uses the same address; future subsidiaries (Mesedi, LLC, etc.) will use the same address
- **Privacy-preserving** — keeps the founder's personal residence (Palm Bay, FL) off all public records (USPTO filings, WHOIS, state business registrations, vendor W-9s)
- **Operationally consolidated** — one physical mailbox to check; mail-scan service forwards images of inbound mail to email; physical forwarding available on request
- **Geographic alignment** — Florida location matches the founder's state of residence (relevant for foreign-LLC qualification questions if any)

## Distinction from Delaware Registered Agent address

**Do not confuse these two address types:**

| Address type | Use case | Canary Systems |
|---|---|---|
| **Mailing address** (this document) | WHOIS, vendor mail, statements, registrar correspondence, operational mail | `2903 West New Haven Ave, #684, West Melbourne, FL 32904` |
| **Registered Agent address** (separate from CMRA) | Service of process, state filings, Delaware corporate records | Stripe Atlas-provided Delaware registered agent (e.g., `131 Continental Dr, Suite 305, Newark, DE`) — different field on Mercury and most legal documents |
| **Founder's residential address** (private) | Personal KYC only — Mercury's "where do you live?" field, IRS Form SS-4 physical-location-of-business field | `545 Castana Ave SE, Palm Bay, FL 32909-4530` — never on public records |

## Form 1583 update required at Anytime Mailbox

The single physical mailbox can serve multiple entities, but each entity must be added as an authorized recipient via a separate USPS Form 1583. This is a one-time 10-minute process per entity:

- [x] Robert J. Canario (personal) — already on file
- [x] Verdifax, LLC — already added during Verdifax setup
- [ ] **Canary Systems, LLC** — add when the LLC is formed (will require: Form 1583 with the new entity name, photo ID, LLC formation documents from Stripe Atlas)
- [ ] **Mesedi, LLC** — add when the LLC is formed (same process)
- [ ] Future subsidiaries — add as formed

**Anytime Mailbox does not charge per additional authorized entity** — it's the same physical mailbox at a fixed monthly rate. Adding entities is paperwork, not additional cost.

## Where this address appears for Canary Systems, LLC (planned)

When the LLC is formed:

- **USPTO trademark filing** for "CANARY SYSTEMS" (Class 36 financial / holding services, possibly Class 42 for tech services) — mailing address field
- **Delaware Certificate of Formation** — entity's mailing address line (separate from the registered agent's Delaware address)
- **IRS Form SS-4** (EIN application) — mailing address; founder's residential goes in the separate "physical location of business" field
- **Mercury bank account application** — business mailing address (Delaware registered agent goes in the legal-address field)
- **Operating Agreement** — entity's principal place of business
- **Vendor W-9 forms** — return mailing address for tax-related correspondence
- **Domain WHOIS records** (if canarysystems.com / canary.systems are registered) — registrant address
- **Any subsidiary's APA / contracts** — when a subsidiary later transacts on behalf of the holding company

## Privacy posture summary

The founder's actual residence (Palm Bay) **never appears** on:
- Public records (USPTO, WHOIS, state filings)
- Vendor-facing documents (W-9s, contracts)
- Marketing or product surfaces
- Domain registrations
- Acquisition documents shared with prospective buyers

The founder's actual residence **does appear** on (limited, private contexts only):
- Personal KYC fields at Mercury (where do *you* live)
- IRS Form SS-4 "physical location of business" field (separate from the entity's mailing address)
- Personal-name signature blocks on inter-entity transfer agreements (e.g., the Domain Assignment Agreement from Robert J. Canario → Verdifax, LLC dated 2026-05-13)

This separation is the cleanest privacy posture for a solo founder operating multiple entities under remote / nomadic conditions.
