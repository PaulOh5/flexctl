# 30-second sales demo runbook

What sales says + clicks to turn a flexctl-equipped GPU server into
"I want this on every box we ship."

The whole demo runs from a single laptop on the customer's tailnet
(or the vendor's demo tailnet for prospect calls).

## The pitch (one breath)

> The box you bought ships with a control plane. Five of your researchers
> can be SSH'ing into their own GPU container in under five minutes,
> with nothing they need to set up locally beyond Tailscale.

Then click the demo. Don't keep talking; the contrast does the work.

## One-time pre-demo setup (do this before walking into the room)

These run once per box and once per laptop. Do them the day before, not
in front of the customer.

```sh
# 1. The box has flexctl running, OAuth wired, prewarmed image present.
ssh root@<demo-host>
systemctl is-active flexctl                                  # active
curl -fsS http://127.0.0.1:8080/healthz | jq -r '.gpu_count' # > 0
docker images --filter reference=nvidia/cuda                  # has 12.4.1-base

# 2. Your laptop is on the demo tailnet and resolves the box.
tailscale status | grep flexctl-demo

# 3. The demo browser tab is at http://<demo-host>:8080 and you are
#    NOT signed in (so the empty state shows the big CTA).
```

The demo flow assumes there are no other environments running. If the
last demo left one behind, stop it from the dashboard before the
customer walks in. Empty state is half the demo.

## The demo (target: ≤ 30 seconds clicking + ≤ 30 seconds first-SSH)

### 0. Frame (talking, no clicks) — 5 seconds

Pull up the browser tab already loaded to `http://<host>:8080`. The
empty state hero is showing.

> "This is the control plane that ships pre-installed on the box you
> just bought. I'll add a researcher and create their isolated GPU
> environment."

### 1. Add the researcher — 8 seconds

- Big CTA in the empty hero says **"+ New environment"**. Don't click
  yet — first add a user.
- Top right: **"Pick user"** → **"+ Add user"**
- Email: `demo@<customer-domain>` → **Create user**
- The dashboard reloads on this new user. Empty state again.

### 2. Spin the environment — 12 seconds

- Click **"+ New environment"**.
- Image dropdown: leave the recommended row checked
  (`nvidia/cuda:12.4.1-base-ubuntu22.04` — pre-warmed on the host).
- Paste your laptop's `~/.ssh/id_ed25519.pub` into the SSH key field.
  (You did this once on your demo laptop; muscle memory ⌘V here.)
- Click **"Create environment"**.
- Browser lands on the environment detail page in `PENDING`.

### 3. Watch the state change — about 10 seconds

The detail page meta-refreshes every 2 seconds. While it does:

> "The platform just allocated a GPU, minted a single-use Tailscale
> identity for this environment, started a hardened container pair,
> and pinned the GPU by UUID so a host reboot can't redirect access."

The badge moves `PENDING` → `RUNNING`. Don't read the bullets
verbatim; the visual transition is the punchline.

### 4. The 1-click SSH — 5 seconds

- The detail page now shows a single SSH command auto-targeted to your
  OS. Hit the **Copy** button next to it.
- Switch to the terminal already open on the laptop. Paste. Enter.
- Inside the work container, type `nvidia-smi`. The customer's GPU
  shows up as the only device the container can see, pinned to the
  UUID the dashboard listed.

> "That's it. The researcher is in their isolated GPU environment from
> any device on their tailnet. From box-arrival to first `nvidia-smi`
> is the demo you just watched."

## Common questions during the demo

| customer asks                                           | answer                                                                                                       |
|--------------------------------------------------------|--------------------------------------------------------------------------------------------------------------|
| "Do my researchers see each other's GPUs?"              | No. Each environment is pinned to one GPU UUID and runs in its own user namespace + Tailscale identity.       |
| "What if a researcher's container crashes?"             | The reconciler sees it within 5 seconds, releases the GPU, and surfaces the docker exit reason in the UI.   |
| "Does this work without internet?"                      | The control plane runs offline. Tailscale needs internet for the initial node registration; offline tailnets work after that. |
| "What about multi-GPU training jobs?"                   | Phase 1 is one GPU per environment. Multi-GPU on a single node is on the roadmap; cross-node is Phase 2.    |
| "What happens when we add a sixth person?"              | The dashboard rejects with a clear "this box is sized for 5 users" message. We sell you another box.        |
| "Is this open source?"                                  | The control plane is yours per box; the source ships with the unit so you're not locked to our binary.       |

## What to do when the demo breaks

| symptom                                  | recovery                                                                |
|------------------------------------------|--------------------------------------------------------------------------|
| Dashboard does not load                  | `systemctl restart flexctl`; while it restarts, talk through the architecture slide. |
| `PENDING` does not flip to `RUNNING`     | Open the **Last exit reason** card; that text is the actual error. Image pull issue is the most common. |
| SSH command times out                    | The customer's tailnet ACL is rejecting the new node. Show them the ACL block in `docs/SHIPPING.md`. |
| You panic and click the wrong thing      | Hit **Stop** on whatever environment is in trouble; reload `/`. Empty state is the natural restart. |

## What to never do during the demo

- Do not paste a customer SSH key live. Use your demo laptop's key,
  always; you saved it at setup.
- Do not show the OAuth `client_secret` on screen. Even if the demo
  laptop is yours, the projector might not be.
- Do not promise "5 minutes from box-arrival" without a prewarmed
  image on the host. First-time pulls of the bigger images
  (`pytorch/pytorch:2.4.0-cuda12.4-cudnn9-runtime`, ~5 GB) blow that
  promise.
