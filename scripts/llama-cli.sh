llama cli -hf unsloth/Qwen3.8-27B-GGUF:IQ4_XS   --gpu-layers 999   --threads 8   --batch-size 512      --cache-type-k q4_0   --cache-type-v q4_0   --no-warmup --ctx-size 32768


llama cli -hf unsloth/Qwen3.8-27B-GGUF:IQ4_XS   --gpu-layers 999   --threads 8   --batch-size 512     --flash-attn on --spec-draft-n-max 2   --cache-type-k q4_0   --cache-type-v q4_0   --ctx-size 32768   --no-warmup

llama cli -hf unsloth/Qwen3.8-27B-GGUF:IQ4_XS   --gpu-layers 999   --threads 8   --batch-size 512     --flash-attn on --spec-draft-n-max 2   --cache-type-k q5_0 --cache-type-v q4_1 --spec-type draft-mtp,ngram-mod --ctx-size 1131072

# best
llama-cli   -hf unsloth/Qwen3.8-27B-GGUF:IQ4_XS   --gpu-layers 999   --threads 8   --batch-size 256   -c 32768   --flash-attn auto   --cache-type-k q4_0   --cache-type-v q4_0   --spec-type none   --no-warmup
