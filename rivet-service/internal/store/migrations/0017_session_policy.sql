-- Доставка политики runner'ам (change add-policy-delivery, спека
-- access-policy).

-- Версия политики, доставленная стадии: сессия — запись о стадии, и её
-- итог привязан к политике, по которой работал агент, независимо от того,
-- чем стадия закончилась (успех, отказ review, провал проверок).
ALTER TABLE sessions ADD COLUMN policy_hash text NOT NULL DEFAULT '';
