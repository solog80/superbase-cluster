-- 011_chat_delete_chat.sql
-- Add per-program "keep chat history" flag to epg_programs, mirroring the
-- Firebase EPG lineup `deleteChat` field. When a program sets
-- delete_chat = false, the chat room managers keep the room's messages across
-- days instead of clearing them at each new-day boundary (default: clear).
--
-- Semantics (matches functions/src/scheduledTvChatManager.js):
--   delete_chat IS NULL  -> clear messages each new day (default)
--   delete_chat = true   -> clear messages each new day
--   delete_chat = false  -> KEEP chat history across days
--
-- Run on the Supabase primary (us1 / Edge) via psql.

alter table public.epg_programs
  add column if not exists delete_chat boolean;
