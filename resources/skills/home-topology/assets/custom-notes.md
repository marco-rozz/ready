# Custom Home Topology Notes

Use this file to define human-friendly nicknames, behavioral overrides, or specific room rules.

Example:
- Bedside Lamp -> light.bedroom_nightstand
- "The main light in the kitchen" -> light.kitchen_main
- Sensor X is unreliable above 40C, please ignore spikes.

Fill it in with the mappings of your own home; the daemon reads it as the
highest-priority override for entity resolution.
