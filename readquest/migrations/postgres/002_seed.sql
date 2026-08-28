-- Demo seed data.
--
-- ON CONFLICT DO NOTHING throughout, so this re-runs harmlessly alongside
-- 001_schema.sql on every startup.

INSERT INTO classes (name) VALUES ('Room 12 - Grade 4')
    ON CONFLICT (name) DO NOTHING;

INSERT INTO teachers (name, class_id)
SELECT 'Ms. Rivera', id FROM classes WHERE name = 'Room 12 - Grade 4'
    ON CONFLICT (name) DO NOTHING;

-- Three students with deliberately different activity profiles, so the
-- at-risk dashboard has something to rank on demo day. Their sessions are
-- logged live during the demo rather than seeded.
INSERT INTO students (name, class_id)
SELECT s.name, c.id
FROM (VALUES ('Maya Chen'), ('Diego Ramirez'), ('Amara Okafor')) AS s(name)
CROSS JOIN classes c
WHERE c.name = 'Room 12 - Grade 4'
    ON CONFLICT (class_id, name) DO NOTHING;

-- Twelve titles spanning seven genres — enough spread that Genre Explorer
-- (3+ distinct known genres) is reachable in a demo-length session.
INSERT INTO books (title, author, genre, age_min, age_max, description) VALUES
    ('Charlotte''s Web',                  'E.B. White',         'Classic',            8,  12, 'A pig named Wilbur is saved by a clever spider.'),
    ('The Very Hungry Caterpillar',       'Eric Carle',         'Picture Book',       3,   6, 'A caterpillar eats through the week and becomes a butterfly.'),
    ('Where the Wild Things Are',         'Maurice Sendak',     'Picture Book',       4,   8, 'Max sails to an island of wild things and is crowned king.'),
    ('Matilda',                           'Roald Dahl',         'Fantasy',            8,  12, 'A brilliant girl with telekinetic powers takes on cruel adults.'),
    ('The Lion, the Witch and the Wardrobe', 'C.S. Lewis',      'Fantasy',            8,  12, 'Four siblings find a wardrobe that opens into Narnia.'),
    ('Holes',                             'Louis Sachar',       'Adventure',         10,  14, 'A boy digs holes at a desert camp and unearths a family curse.'),
    ('Bridge to Terabithia',              'Katherine Paterson', 'Realistic Fiction',  9,  12, 'Two friends invent a magical kingdom in the woods.'),
    ('Frog and Toad Are Friends',         'Arnold Lobel',       'Early Reader',       4,   8, 'Five gentle stories about two devoted friends.'),
    ('The Wild Robot',                    'Peter Brown',        'Science Fiction',    8,  12, 'A robot washes ashore on a wild island and learns to survive.'),
    ('Because of Winn-Dixie',             'Kate DiCamillo',     'Realistic Fiction',  8,  12, 'A stray dog helps a lonely girl find her place in a new town.'),
    ('Wonder',                            'R.J. Palacio',       'Realistic Fiction', 10,  14, 'A boy with a facial difference starts mainstream school.'),
    ('Hatchet',                           'Gary Paulsen',       'Adventure',         10,  14, 'A boy survives alone in the wilderness after a plane crash.')
    ON CONFLICT (lower(title)) DO NOTHING;

INSERT INTO badges (name, description, condition_type, condition_value) VALUES
    ('First Step',     'Logged your very first reading session',        'first_session', 1),
    ('Page Turner',    'Read 100 pages in total',                       'pages_total', 100),
    ('Bookworm',       'Read 500 pages in total',                       'pages_total', 500),
    ('Week Warrior',   'Read on 7 consecutive days',                    'streak',        7),
    ('Genre Explorer', 'Read books across 3 different genres',          'genres',        3)
    ON CONFLICT (name) DO NOTHING;
