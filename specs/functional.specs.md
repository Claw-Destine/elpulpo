# El Pulpo functional specs

## Proxy

### Configuration

All the configuration should be possible through the web dashboard without restarting the service. The structure should be as follows (using yaml for a convenient hierarchical presentation):

```yaml
-   host: <ip or hostname>
    servers:
    -   port: <server port>
        api: <for example 'openai' or 'ollama'>
        prefix: <prefix for models served by this server>
        postfix: <postfix for models served by this server>
        show: <should the server be proxied directly. Default is true. If false the models from this service may still be proxied via loadbalancer>
```

The host+port and prefix+postfix combination should be unique

### Behavior

El Pulpo allows for a configuration of multiple servers and hosts. Let's imagine following configuration:

```yaml
Servers:
-   host: 192.168.1.101
    servers:
    -   port: 11434
        api: ollama
        prefix: "comp1-"
        postfix: "-ollama"
    -   port: 8000
        api: openai
        prefix: "comp1-"
        postfix: "-vllm"
- host: 192.168.1.102
    servers:
    -   port: 11434
        api: ollama
        prefix: "comp2-"
        postfix: "-ollama"
```

And the servers return following models:

1. Server at 192.168.1.101:11434:

    - qwen3.8:27b
    - gemma4:31b

2. Server at 192.168.1.101:8000:

    - deepseek-v4-flash

3. Server at 192.168.1.102:11434:

    - gpt-oss:120b
    - qwen3.8:27b

El Pulpo would return a following list:

- comp1-qwen3.8:27b-ollama
- comp1-gemma4:31b-ollama
- comp1-deepseek-v4-flash-vllm
- comp2-gpt-oss:120b-ollama
- comp2-qwen3.8:27b-ollama

El Pulpo would periodically test if the servers are up and only return models that are currentl available.

## Token usage stats

El Pulpo's dashboard should allow user to view token usage stats as a list. User should be able to filter by:

- server
- model
- period (from - to)

After every refresh el pulpo should calculate sum of tokens for currently displayed list. List should be paged if too long.

In this view user should be able to enter cost per 1m tokens. If the value is set the cost should show next to token sum.

## Load balancing

### K/V cache support

Load balancing should allow selecting preferred server and be K/V cache aware. Upon request it should hash each message in the stack and store as hash record:

- stack hash (string) - explanation below
- last-used (timestamp) - request timestamp
- expiry (timestamp) - request timestamp + cache_expiry for the model.
- model (string)

Stack hash is calculated for each message in the context and should be calculated from concatenation of current message and hash of the previous message. Let's call them HASH_0, HASH_1, HASH_2, .... They should be calculated as follows:

- HASH_0 = hash(messages[0])
- HASH_1 = hash(HASH_0 + messages[1])
- HASH_2 = hash(HASH_0 + messages[2])
- ...

After each request load balancer should look up all matching hashes in cache and if hash is not expired route to the server with latest matching hash timestamp. After that it should only upser all hash records into the cache

### Configration

Let's go through this example (this is just a representation, it should be configured via web dashboard):

```yaml
Loadbalancer:
-   name: qwen3.8:27b
    models:
    -   id: comp1-Qwen/Qwen3.8-27B-FP8-vllm
        priority: 10
        concurent: 4
        min_queue: 0
        cache_expiry: 86400
        cache_limit: 1000
    -   id: comp2-qwen3.8:27b-ollama
        priority: 0
        concurent: 1
        min_queue: 4
        cache_expiry: 600
        cache_limit: 100
```

We have 2 models on 2 different servers:

- `comp1-Qwen/Qwen3.8-27B-vllm` - this is a machine we prefer (because it's faster for example)
- `comp2-qwen3.8:27b-ollama` - this is the fallback machine.

So the rules are:

1. If there is no cache hit the load balancer should prefer the model with the highest priority.
2. Each model has an amount of slots configured by `concurent` setting.
3. Requests should be routed proportionally to the concurent values starting from the highest priority server.
4. If the higher priority model has less request than its `concurent` value we route to it even if there is cache hit on lower priority server but only if there is less messages in context than priority difference. Example: model A has priority 10 and model B has priority 2. If the model A has available slots and context amount of messages is less than 8 (PRIO_A - PRIO_B) we route to model A. If the amount of messages is equal or larger to 8 we stick with model B.
5. El Pulpo will clean the cache in regular intervals (configured by env variable):
    - it will remove all the hash records older than expiry date.
    - it will remove oldest hash records if there is more records for model thatn `cache_limit`
