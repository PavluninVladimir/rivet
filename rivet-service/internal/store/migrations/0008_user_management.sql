-- Роль участника проекта и обязательная смена пароля
-- (change add-user-management, спеки domain-model и access-policy).

-- Роль членства: owner управляет проектом, member работает с задачами.
-- Существующие участники получают owner: до этой миграции настройки проекта
-- мог менять любой участник, и понижение всех до member оставило бы проекты
-- без владельца (design, план миграции).
ALTER TABLE project_members
    ADD COLUMN role text NOT NULL DEFAULT 'member'
        CHECK (role IN ('owner', 'member'));

UPDATE project_members SET role = 'owner';

-- Признак обязательной смены пароля: поднимается сбросом со стороны
-- администратора, снимается сменой пароля владельцем учётной записи.
ALTER TABLE users
    ADD COLUMN must_change_password boolean NOT NULL DEFAULT false;
