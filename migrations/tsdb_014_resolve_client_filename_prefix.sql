-- tsdb_014_resolve_client_filename_prefix.sql
-- Mirror the Go resolver in SQL: support the studio naming convention
-- "Client - Ad Name.mp3" (advertiser = text before the first " - ").
-- Resolution order: explicit client → rule pattern → filename prefix →
-- 'Other Advert' / 'Unknown'.
-- Run on TSDB: analytics @ 100.116.185.70:55439.

create or replace function public.resolve_traffic_client(
  p_title           text,
  p_filename        text,
  p_explicit_client text default null
) returns text
language plpgsql stable as $$
declare
  v_client text;
  v_target text;
  v_base   text;
  v_prefix text;
begin
  if p_explicit_client is not null and trim(p_explicit_client) <> '' then
    return trim(p_explicit_client);
  end if;

  v_target := coalesce(p_title, '') || ' ' || coalesce(p_filename, '');

  -- 1. Known advertiser rules.
  select client_name into v_client
  from public.traffic_client_rules
  where v_target ~* match_pattern
  order by priority desc, id asc
  limit 1;

  if v_client is not null then
    return v_client;
  end if;

  -- 2. "Client - Ad Name.mp3" convention: advertiser = prefix before first " - ".
  v_base := regexp_replace(coalesce(p_filename, ''), '^.*[/\\]', '');
  v_base := regexp_replace(v_base, '\.(mpeg\.mp3|mp3\.mpeg|mp3|mpeg|wav|mp4|aac|m4a|ogg)$', '', 'i');
  v_prefix := btrim(regexp_replace(v_base, ' - .*$', ''));

  if position(' - ' in v_base) > 0 and v_prefix <> ''
     and length(v_prefix) between 2 and 40
     and lower(v_prefix) not in ('ad','ads','advert','adverts','advertisement',
                                 'jingle','promo','spot','commercial','sting',
                                 'announcement','audio','voice','track')
     and v_prefix ~ '[A-Za-z]' then
    -- Prefer the canonical spelling if this prefix is a known advertiser.
    select client_name into v_client
    from public.traffic_client_rules
    where lower(client_name) = lower(v_prefix)
    limit 1;

    if v_client is not null then
      return v_client;
    end if;

    return v_prefix;
  end if;

  -- 3. Generic fallback.
  if v_target ~* 'adverts' or v_target ~* 'commercial' then
    return 'Other Advert';
  end if;

  return 'Unknown';
end;
$$;
