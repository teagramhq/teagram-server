ALTER TABLE public.messages
    ADD COLUMN grouped_id BIGINT;

ALTER TABLE public.channel_messages
    ADD COLUMN grouped_id BIGINT;
