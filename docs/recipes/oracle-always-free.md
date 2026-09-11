# Oracle Cloud, Always Free

Cost $0 inside the Always Free limits; a reserved public IP that
survives the VM; x86_64 (`VM.Standard.E2.1.Micro`); systemd via
cloud-init. Follow [the cloud VM recipe](cloud-init.md); these are the
Oracle-specific parts, and the first one is the load-bearing one.

## Convert the account to Pay As You Go first

Not for capacity. Oracle **reclaims idle Always Free instances** on
free-tier accounts: over a 7-day window, CPU at the 95th percentile
under 20 % and network under 20 % marks the instance idle and it is
stopped. A collector fails every idle criterion comfortably — it sleeps
between fetches and moves a few kilobytes a minute. Upgrading to PAYG
stops reclamation and costs nothing while you stay inside the Always
Free limits (a card is required; set a **$1 budget alert** under
Billing → Budgets so any surprise is a notification, not a bill).

## Create

- **Shape:** `VM.Standard.E2.1.Micro` (AMD, 1/8 OCPU, 1 GB). It
  provisions reliably. The Ampere `A1.Flex` shape is also free and
  bigger, but "Out of host capacity" is common and the collector does
  not need it.
- **Image:** Oracle Linux 9 (cloud-init present; Python 3.9 if you want
  the Python twin). Ubuntu works too.
- **Networking:** create the instance with a public IP **ephemeral**,
  then swap it for a reserved one (below). Or pick "No public IP" at
  create and attach the reserved IP afterwards — either way.
- **Advanced options → Management → cloud-init script:** paste
  `cloud-init.yaml` with your two secrets filled in.

## Reserve the IP

Networking → IP Management → **Reserved Public IPs** → Reserve. Then on
the instance: Attached VNICs → the VNIC → IPv4 Addresses → Edit → choose
"Reserved public IP" and pick it. This is the address for your Clash
Royale key, and it outlives the instance: destroy and rebuild as often
as you like.

## Things that surprise people

- **`ip addr` inside the instance shows only `10.0.0.x`.** Oracle does
  1:1 NAT; the VM never sees its public address. This is normal. The
  `egress` line of `collector doctor` is the address as the outside
  world sees it, which is what the key allowlist needs.
- **Home region cannot be changed after signup**, and Always Free
  shapes live in the home region. Pick it deliberately (nearest you;
  the collector does not care).
- **Egress is open by default;** the collector needs only outbound
  HTTPS, so the default security list works. Nothing inbound is needed
  beyond SSH.
- **Two Always Free micro instances per account** is the limit for
  E2.1.Micro. Two collectors on two VMs behind two reserved IPs is a
  fine use of both.
