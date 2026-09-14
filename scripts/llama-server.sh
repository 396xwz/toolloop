CONTEXT=65536
llama-server \
  -hf unsloth/Qwen3.8-27B-GGUF:IQ4_XS \
  -c $CONTEXT \
  --gpu-layers 999 \
  --threads 8 \
  --flash-attn auto   --cache-type-k q4_0   --cache-type-v q4_0   --spec-type none   --no-warmup \
  --batch-size 256 \
  --host 0.0.0.0 \
  --port 8080


#llama-server   -hf unsloth/Qwen3.8-27B-GGUF:IQ4_XS   --gpu-layers 999   --threads 8   --batch-size 256   --ctx-size 32768   --cache-type-k q4_0   --cache-type-v q4_0   --no-warmup   --flash-attn auto \
#  --no-mmap   --no-kv-offload


# llama-server    -hf unsloth/Qwen3.8-27B-GGUF:IQ4_XS   --no-mmproj-offload   -c 149504   --temp 0.7   --top-p 0.8   --top-k 20   --min-p 0.0   --repeat-penalty 1.08   --reasoning on   --chat-template-kwargs "{\"reasoning_effort\":\"medium\"}"   --n-gpu-layers 999   --flash-attn  auto --parallel 1   --cache-type-k q4_0   --cache-type-v q4_0   --spec-type draft-mtp   --spec-draft-n-max 2   --batch-size 512   --ubatch-size 256   --host 127.0.0.1   --port 8080
