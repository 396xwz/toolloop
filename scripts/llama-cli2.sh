llama cli -hf unsloth/Qwen3.8-27B-GGUF:IQ4_XS   --gpu-layers 999   --threads 8   --batch-size 512      --cache-type-k q4_0   --cache-type-v q4_0   --no-warmup --ctx-size 32768


llama cli -hf unsloth/Qwen3.8-27B-GGUF:IQ4_XS   --gpu-layers 999   --threads 8   --batch-size 512     --flash-attn on --spec-draft-n-max 2   --cache-type-k q4_0   --cache-type-v q4_0   --ctx-size 32768   --no-warmup

llama cli -hf unsloth/Qwen3.8-27B-GGUF:IQ4_XS --n-gpu-layers 999 \
  --override-tensor 'blk\.(0|1|2|3|4|5|6|7|8|9)\.ffn_.*=CPU' \
  --no-mmap \
  \
  --ctx-size 65536 \
  --flash-attn on \
  --cache-type-k q8_0 --cache-type-v q8_0 \
  --cache-reuse 256 \
  --parallel 1 --cont-batching 0 \
  \
  --spec-type draft-mtp --spec-draft-n-max 2 \
  --cache-type-k-draft q8_0 --cache-type-v-draft q8_0 \
  \
  --threads 7 --threads-batch 8 \
  --batch-size 1024 --ubatch-size 512 \
  \
  --jinja --reasoning-format deepseek --reasoning-preserve \
  --reasoning-budget 5000 \
  \
  --temp 1.0 --top-p 0.95 --top-k 20 --min-p 0.0 \
  --presence-penalty 0.0 --repeat-penalty 1.0
