# Hetzner Cloud

A few euros a month for the smallest shape; a primary IPv4 that persists
independently of the server; x86_64 or arm64 (the `CAX` shapes are arm64
and cheapest); systemd via cloud-init. Follow
[the cloud VM recipe](cloud-init.md); the Hetzner parts:

- **Primary IP first.** Cloud console → Primary IPs → Create, IPv4, in
  the location you will use, unassigned. It bills separately from the
  server, which is the point: delete and rebuild the server and the
  address stays yours.
- **Create the server** with that primary IP selected under Networking
  (untick the auto-assigned one), any Linux image, the smallest shape.
  Paste `cloud-init.yaml` into the **Cloud config** box on the create
  form.
- The installer picks the arm64 binary on a `CAX` shape by itself; the
  YAML needs no edit.
- Hetzner's default firewall is none; the collector needs only outbound
  HTTPS, so a firewall allowing inbound SSH only is a good idea and
  changes nothing for the collector.
