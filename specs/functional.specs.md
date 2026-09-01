# El Pulpo functional specs

## Proxy

### Configuration

All the configuration should be possible through the web dashboard without restarting the service. The structure should be as follows (using yaml for a convenient hierarchical presentation):

```yaml
-   host: <ip or hostname>
    id: <short host name>
    description: <String that describes the host.>
    servers:
    -   port: <server port>
        api: <for example 'openai' or 'ollama'>
        id: <short server name>
        description: <String that describes the server.>
        show: <should the server be proxied directly. Default is true. If false the models from this service may still be proxied via loadbalancer>
```

For hosts all host adresses and ids should be unique. For servers ports and ids should be unique within signle host.

### Behavior

El Pulpo allows for a configuration of multiple servers and hosts. Let's imagine following configuration:

```yaml
Servers:
-   host: 192.168.1.101
    id: minion1
    servers:
    -   port: 11434
        api: ollama
        id: "id"
    -   port: 8000
        api: openai
        postfix: "vllm"
- host: 192.168.1.102
  id: minion2
    servers:
    -   port: 11434
        api: ollama
        id: "ollama"
```

And the servers return following models:

1. Server at 192.168.1.101:11434 (minion1, ollama):

    - qwen3.8:27b
    - gemma4:31b

2. Server at 192.168.1.101:8000 (minion1, vllm):

    - deepseek-v4-flash

3. Server at 192.168.1.102:11434 (minion2, ollama):

    - gpt-oss:120b
    - qwen3.8:27b

El Pulpo would return a following list:

- qwen3.8:27b-ollama@minion1
- gemma4:31b-ollama@minion1
- deepseek-v4-flash-vllm@minion1
- gpt-oss:120b-ollama@minion2
- qwen3.8:27b-ollama@minion2

El Pulpo would periodically test if the servers are up and only return models that are currentl available.

## Token usage stats

El Pulpo's dashboard should have statistics tab that allows user to view token usage stats in a form of table with following columns (spreadsheet.).

- host
- server
- model
- timestamp
- tokens in
- tokens out

User should be able to filter by each field and set up time period. There should be some ready to use time periods:

- today
- this month

### Savings

In this view user should be able to enter cost per 1m tokens. If the value is set the cost should show next to token sum so the user can estimate how much they saved on MaaS services.
