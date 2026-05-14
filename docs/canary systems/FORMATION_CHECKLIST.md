# Canary Systems, LLC — Formation Checklist

**Purpose:** Step-by-step playbook for forming Canary Systems, LLC as the parent holding company for all post-Verdifax projects (Mesedi, LLC and future subsidiaries). Each section is gated by dependencies — do not skip ahead.

**Recommended timing:** form Canary Systems IMMEDIATELY before forming Mesedi, LLC — i.e., right after Verdifax LOI signs and you're ready to begin Mesedi development. Forming earlier means paying for an entity that does nothing until subsidiary work begins; forming later means scrambling to set up parent + subsidiary in parallel under time pressure.

**Estimated total time from "decide to form" to "fully operational holding co with Mesedi as first subsidiary":** ~10 business days, mostly waiting on third-party SLAs (Stripe Atlas filing 1-2 days, IRS EIN issuance same-day to 1 day, Mercury approval 1-2 days with priority transfer).

**Estimated upfront cost:** ~$1,000 for parent + first subsidiary combined.

- Stripe Atlas Canary Systems, LLC formation: $500
- Stripe Atlas Mesedi, LLC formation (as subsidiary): $500
- USPTO trademark filing for "CANARY SYSTEMS" (Class 36): $350 (TEAS Standard) or $250 (TEAS Plus, restricted vocabulary)
- Optional: USPTO trademark filing for the logo design + word mark combined: another $250-350

**Estimated annual ongoing cost:** ~$800/year for parent + Mesedi combined.

- Delaware franchise tax: $300/year per LLC × 2 = $600
- Delaware registered agent: $100/year per LLC × 2 = $200 (free year 1 via Stripe Atlas)
- USPTO trademark maintenance (every 6 / 10 years): trivial

---

## Section A — Pre-formation verification (1-2 days, do before spending any money)

These checks cost nothing and prevent costly retroactive changes after formation.

- [ ] **USPTO trademark search for "CANARY SYSTEMS"**
  - [ ] TESS search at `tmsearch.uspto.gov` for exact match "CANARY SYSTEMS"
  - [ ] Search for partial matches: "CANARY", "CANARYSYS", related variants
  - [ ] Focus on Class 36 (financial services / holding companies) and Class 42 (computer services / SaaS) — these are the most relevant classes for a tech holding company
  - [ ] Document the search results (screenshots) — these become part of the data room when subsidiary acquirers do DD
  - [ ] If conflicts exist in your target classes: pivot the name BEFORE filing the Delaware LLC. Easier to change the name now than after formation paperwork.
- [ ] **Delaware LLC name availability check**
  - [ ] Search at `icis.corp.delaware.gov` (Department of State entity search) for "Canary Systems"
  - [ ] If taken: try "Canary Systems Group, LLC" / "Canary Systems Holdings, LLC" / "Canary Systems Capital, LLC" (one of these will be available)
  - [ ] If taken AND USPTO conflict exists: serious name reconsideration warranted
- [ ] **Domain availability check** (optional but recommended)
  - [ ] `canarysystems.com` — likely premium / squatted (check at any registrar)
  - [ ] `canarysystems.ai` — likely available, $80/year via Cloudflare Registrar
  - [ ] `canary.systems` — likely available, ~$25/year
  - [ ] Pick at least one (.ai or .systems is sufficient — holding companies rarely need .com)
- [ ] **Google Workspace alias verification**
  - [ ] Decide where Canary Systems email will route — under existing `verdifax.com` workspace, OR under a new `canarysystems.ai` (or `canary.systems`) workspace
  - [ ] Recommended: separate workspace for clean entity separation. Cost: $6/user/month for Google Workspace Starter. Aliases to set up: `hello@`, `legal@`, `banking@`, `team@`
- [ ] **Logo trademark consideration**
  - [ ] The goldenrod-canary mark is distinctive enough to be filed as a design-mark trademark separately from the wordmark
  - [ ] Design-mark + wordmark combined filing = stronger protection (covers both the visual and the name)
  - [ ] Consider deferring the design-mark filing until you have meaningful business activity under the brand — USPTO requires "use in commerce" for filed marks; if Canary Systems sits idle for 6 months, the design mark could be challenged

**Acceptance:** all three name surfaces (USPTO trademark, Delaware LLC, domain) confirmed available for "Canary Systems" before any money is spent on formation.

---

## Section B — LLC formation via Stripe Atlas (1-2 days)

- [ ] **Stripe Atlas: form Canary Systems, LLC**
  - [ ] Initiate from existing Stripe Atlas account (the same one used for Verdifax)
  - [ ] Entity type: Delaware single-member LLC
  - [ ] Sole member: Robert J. Canario (individual; will NOT be Canary Systems holding any of itself)
  - [ ] State of formation: Delaware
  - [ ] Operating Agreement template: Stripe Atlas single-member default — fine for this use case
  - [ ] Registered Agent: Stripe Atlas-bundled Delaware agent for year 1; renewable separately ~$100/year thereafter
- [ ] **Required during the Stripe Atlas filing**
  - [ ] Founder personal info: name, DOB, SSN, residential address (Palm Bay — private), founder's personal email
  - [ ] Business mailing address: 2903 West New Haven Ave #684, West Melbourne, FL 32904 (the Anytime Mailbox CMRA)
  - [ ] Business description: "Holding company for investments in software and technology subsidiaries"
  - [ ] NAICS code: 551112 (Offices of Other Holding Companies) — correct classification for a non-financial holding company
- [ ] **Receive Certificate of Formation from Delaware**
  - [ ] Stripe Atlas typically delivers within 1-2 business days
  - [ ] Save the PDF to `mesedi/docs/canary systems/formation-docs/` (create the subfolder)
- [ ] **Sign and store the Operating Agreement**
  - [ ] Stripe Atlas provides a single-member template
  - [ ] Sign via DocuSign (same workflow as the Domain Assignment Agreement)
  - [ ] Save signed PDF
- [ ] **Receive EIN from IRS**
  - [ ] Stripe Atlas auto-submits Form SS-4 on your behalf
  - [ ] SLA: same-day to 1-2 business days
  - [ ] You'll receive an email from Stripe with the EIN
  - [ ] Save the EIN confirmation in `formation-docs/`
  - [ ] **Treat the EIN as semi-confidential** — corporate SSN-equivalent; not for public posting

**Acceptance:** Canary Systems, LLC formally exists in Delaware with: EIN issued, Operating Agreement signed, registered agent active, certificate of formation in hand.

---

## Section C — Banking + operational setup (2-3 days, runs in parallel with Section B's waiting periods)

- [ ] **Mercury bank account for Canary Systems**
  - [ ] Apply via Stripe Atlas → Banking tab (priority review path)
  - [ ] Initial deposit: $100 founder contribution (same minimum-stress pattern as Verdifax)
  - [ ] Application details (copy-paste from this doc when ready):
    - Legal name: `Canary Systems, LLC`
    - EIN: (from Section B)
    - State of formation: Delaware
    - Industry: "Holding companies and corporate management" / Industry → SaaS subcategory N/A (it's a holding co; pick the most accurate Stripe option)
    - NAICS: `551112`
    - Business description: *"Holding company for investments in software and technology subsidiaries. Initially holds 100% of Mesedi, LLC (agent-reliability infrastructure SaaS). Expected to hold additional subsidiaries as new product ventures are launched."*
    - Expected monthly transaction volume: low ($0-10K typical; occasional larger as subsidiaries distribute profits up to the holding co)
    - Expected monthly balance: $10K-100K (operational; could spike on subsidiary distributions or future acquisition proceeds)
  - [ ] Personal KYC: same as Verdifax (residential address Palm Bay, photo ID, founder cell)
  - [ ] Email for Mercury login: `banking@canarysystems.ai` or `banking@canary.systems` (the new workspace) OR `banking@verdifax.com` if you haven't set up the new workspace yet — can be updated later
- [ ] **Update Anytime Mailbox: add Canary Systems, LLC as authorized recipient**
  - [ ] File USPS Form 1583 with Anytime Mailbox naming Canary Systems, LLC
  - [ ] Provide: photo ID, LLC formation certificate (from Section B), operating agreement
  - [ ] Anytime Mailbox typically processes within 1-2 business days
  - [ ] No additional fee (same mailbox, additional authorized entity)
- [ ] **DNS setup** (if registering canarysystems.ai or canary.systems)
  - [ ] Register domain at Cloudflare Registrar
  - [ ] Configure WHOIS contact: Canary Systems, LLC at the Anytime Mailbox address, founder's personal LLC-purpose phone
  - [ ] Enable WHOIS privacy (Cloudflare default)
  - [ ] DNS records: park initially (no public site needed)
- [ ] **Google Workspace setup** (if creating new workspace for Canary Systems)
  - [ ] Create new Google Workspace under canarysystems.ai or canary.systems
  - [ ] Aliases: `hello@`, `legal@`, `banking@`, `team@`, `noreply@`
  - [ ] Update Mercury login email to `banking@` of the new workspace once set up
- [ ] **Insurance quotes (optional, can defer)**
  - [ ] D&O / cyber / E&O quotes from Vouch (via Stripe Atlas partnership)
  - [ ] Most holding companies don't carry these — subsidiaries do. Bind insurance at the subsidiary level when each one has paying customers.

**Acceptance:** Canary Systems, LLC has an active Mercury bank account, the CMRA accepts mail addressed to it, and (optionally) a domain is registered.

---

## Section D — USPTO trademark filing for "CANARY SYSTEMS" (1 day to file, 8-12 months to grant)

- [ ] **Decide filing path**
  - [ ] **Recommended: TEAS Plus ($250)** — requires choosing goods/services from a controlled vocabulary list. Saves $100 over TEAS Standard. The trade-off is less flexibility in defining the goods/services description, but for a holding company's standard classification this is fine.
  - [ ] Alternative: TEAS Standard ($350) — flexible goods/services description, useful only if your specific description doesn't fit the controlled vocabulary
- [ ] **Class selection**
  - [ ] **Class 36 — Financial services / holding company services** (primary class — this is the holding co's actual business)
  - [ ] Optional: Class 42 (computer services / SaaS) — defensive filing if any subsidiary will ship product under co-branding ("a Canary Systems company") — this protects the mark in the tech-product space where future subsidiaries operate
  - [ ] Multi-class filing is $250-350 per class — total for 2-class filing ~$500-700
  - [ ] **Recommendation: Class 36 only initially** — Class 42 can be added later via separate filing if subsidiary co-branding becomes meaningful
- [ ] **Filing basis**
  - [ ] If Canary Systems has already received its first member contribution and started holding Mesedi, LLC: **Use Section 1(a) "use in commerce"** — same as Verdifax's trademark filing
  - [ ] If filing before first subsidiary is formed: **Section 1(b) "intent to use"** — allows you to file the mark to reserve it now and submit a Statement of Use later when the holding company has actual activity
- [ ] **Mark format**
  - [ ] **Standard character mark for "CANARY SYSTEMS"** — covers the wordmark in any font / styling
  - [ ] Optional: separate design mark filing for the logo (canary + wordmark composite) — adds another $250-350 in fees but protects the visual identity. Defer this until you have meaningful business activity under the brand.
- [ ] **Filing logistics**
  - [ ] Use the founder's existing USPTO.gov account (created for Verdifax trademark)
  - [ ] Correspondence address: same Anytime Mailbox CMRA address used for Verdifax (consistent address-of-record pattern)
  - [ ] Save filing receipt as `CANARY_SYSTEMS_USPTO_RECEIPT.pdf` in `mesedi/docs/canary systems/trademark/` (create subfolder)
- [ ] **Wait for examiner review** (~8-12 months)
  - [ ] You will get scam emails immediately after filing — same pattern as Verdifax (USPTO records are public)
  - [ ] Real USPTO communications come from `@uspto.gov`, route through TSDR — same recognition pattern
- [ ] **Respond to any Office Action if one issues** (within 3 months, free; 6-month extension available)

**Acceptance:** Trademark application filed, receipt saved, USPTO serial number recorded.

---

## Section E — Form the first subsidiary (Mesedi, LLC) under Canary Systems

The whole point of Canary Systems is to hold subsidiaries. This section runs immediately after Sections B and C are complete.

- [ ] **Stripe Atlas: form Mesedi, LLC**
  - [ ] Important: at filing time, Stripe Atlas defaults to single-member LLC with the individual founder as the member. Atlas does NOT directly support "another LLC as the member" at formation. The cleanest workaround:
    - [ ] **Step E1:** Form Mesedi, LLC via Stripe Atlas with Robert J. Canario as the individual sole member
    - [ ] **Step E2:** Immediately after formation, execute a **Membership Interest Assignment Agreement** transferring 100% of Mesedi membership interest from Robert J. Canario (individual) to Canary Systems, LLC (parent). Single-page document, same pattern as the Domain Assignment Agreement for Verdifax.
    - [ ] **Step E3:** Update Mesedi's Operating Agreement to reflect Canary Systems, LLC as the sole member (Stripe Atlas can provide an amended single-member template)
  - [ ] This two-step pattern keeps Stripe Atlas's bundled benefits (perks page, Mercury integration, EIN issuance, registered agent) for Mesedi while ending up with the correct parent-subsidiary structure
- [ ] **Mesedi-specific setup** (parallel to but separate from Canary Systems)
  - [ ] Mesedi EIN issued by IRS
  - [ ] Mesedi Mercury bank account (separate from Canary Systems' Mercury account — no commingling)
  - [ ] Mesedi added to Anytime Mailbox via Form 1583 (third entity at the same physical mailbox)
  - [ ] Mesedi.ai domain transferred from Robert J. Canario (personal) to Mesedi, LLC via a Domain Assignment Agreement (same pattern used for Verdifax domains)
- [ ] **Document the parent-subsidiary relationship**
  - [ ] Sign a one-page Member Contribution Agreement showing Canary Systems made an initial capital contribution to Mesedi (e.g., $1,000 seed capital) — establishes Canary Systems as a real economic parent, not just a paper one
  - [ ] Save in `formation-docs/` for both entities

**Acceptance:** Mesedi, LLC exists in Delaware with Canary Systems, LLC as the sole member, separate EIN, separate Mercury account, separate Operating Agreement.

---

## Section F — Ongoing operational discipline (forever)

The asset-protection value of the parent-subsidiary structure depends entirely on operating each entity as genuinely separate. Three rules to follow forever:

- [ ] **Never commingle funds**
  - [ ] Each entity pays its own bills from its own Mercury account
  - [ ] When Canary Systems contributes capital to a subsidiary: document as a "Member Contribution" with a one-paragraph memo
  - [ ] When a subsidiary distributes profits to Canary Systems: document as a "Member Distribution"
  - [ ] Never pay Mesedi's AWS bill from Canary Systems' card (or vice versa)
- [ ] **Each entity signs its own contracts**
  - [ ] Vendor contracts (AWS, Fly.io, Cloudflare) signed by the operating subsidiary, not by the parent
  - [ ] If a vendor relationship is shared across multiple subsidiaries (e.g., one Anthropic API account billed to Canary Systems but used by both Mesedi and a future subsidiary), execute a written intercompany services agreement documenting the arrangement
- [ ] **Maintain separate books for each entity**
  - [ ] Each entity has its own bookkeeping system (Pilot / Bench / Wave) OR each has its own chart of accounts in a single bookkeeping system
  - [ ] Annual filings: each Delaware LLC files its own franchise tax return ($300/year minimum) by June 1 of the following year
- [ ] **Track membership interests at the parent level**
  - [ ] Canary Systems' Operating Agreement should be amended to list each subsidiary it holds and the % membership held in each
  - [ ] Use a simple cap-table-style document (`SUBSIDIARIES.md` inside `canary systems/`) that tracks: subsidiary name, formation date, % owned, capital contributed, current valuation estimate

## Section G — When a subsidiary exits (the future-state playbook)

When a subsidiary (Mesedi or future) reaches an acqui-IP sale event, the structure handles it cleanly:

- [ ] **Asset purchase pattern (most common):** acquirer wires purchase price to subsidiary's Mercury account; subsidiary distributes proceeds to Canary Systems as a Member Distribution; subsidiary dissolves; Canary Systems holds the cash, can reinvest into next subsidiary or distribute to Robert personally.
- [ ] **Membership-interest purchase pattern (less common):** acquirer buys Canary Systems' membership interest in the subsidiary directly. Cleaner because the subsidiary survives with new ownership and Canary Systems just receives cash. Some acquirers prefer this; others prefer asset purchase.
- [ ] **Tax flow at the parent level:** each subsidiary is disregarded for federal tax → flows through to Canary Systems → which is also disregarded → flows through to Robert personally. Net effect: one consolidated tax event on Robert's personal return. Same shape as if subsidiaries were held personally, but with cleaner liability separation along the way.

The parent-subsidiary structure gives you optionality at exit: you can sell individual subsidiaries without disturbing the others. The structure pays for itself the first time you sell one subsidiary while retaining the others.

---

## Total time + cost summary

| Section | Time | Upfront cost | Annual ongoing |
|---|---|---|---|
| A. Pre-formation verification | 1-2 days | $0 | $0 |
| B. LLC formation via Stripe Atlas | 1-2 days | $500 | $400 ($300 franchise + $100 RA) |
| C. Banking + operational setup | 2-3 days | $0 | $80 (Cloudflare .ai domain if registered) |
| D. USPTO trademark filing | 1 day to file; 8-12 mo to grant | $250 (TEAS Plus, Class 36) | $0 (renewals every 6/10 years) |
| E. Form Mesedi, LLC subsidiary | 2-3 days | $500 | $400 ($300 franchise + $100 RA) |
| F. Ongoing operational discipline | Forever | $0 | varies by bookkeeping choice |
| **Total** | **~10 business days** | **~$1,250** | **~$880 / year** |

The total burn for running a parent-subsidiary structure with Canary Systems + Mesedi is well under $1,000/year ongoing. The Verdifax sale proceeds cover decades of this overhead.

---

**End of formation checklist.** Status: planning document. Activate this checklist immediately after Verdifax LOI is signed and you're ready to begin Mesedi development. Do not form Canary Systems before that — paying $500 for an entity that does nothing for two months is dead capital.
